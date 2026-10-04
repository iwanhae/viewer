"""The embedding pipeline: claim/download on the main thread, embed async.

Downloads and embedding overlap continuously: the main thread claims the
next batch as soon as the previous batch's downloads are consumed (the
server hands out fresh blobs, live leases are never re-claimed), while a
dedicated embed thread works through a bounded queue of preprocessed
pixels. Results are POSTed as they accumulate instead of at batch end.
"""

from __future__ import annotations

import logging
import queue
import random
import threading
import time
from concurrent.futures import TimeoutError as FutureTimeoutError
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone

import httpx
import numpy as np
import torch

from . import api, vectors
from .config import EMBEDDING_DIM, LEASE_RENEW_MARGIN_SECONDS, Config
from .downloads import PermanentError, SkipTransient, download_and_preprocess

log = logging.getLogger(__name__)

RESULTS_CHUNK = 512  # server cap is 1024 items / 16 MB; stay comfortably under
RESULTS_FLUSH_SECONDS = 10.0  # post small result batches after this much time
ERROR_TEXT_LIMIT = 200  # the server keeps 512 bytes of error text; stay short
WAIT_SLICE_SECONDS = 1.0  # idle-sleep granularity for flush/renew upkeep
ENQUEUE_SLICE_SECONDS = 0.5  # backpressure wait; also the renew check cadence


class Worker:
    def __init__(
        self,
        cfg: Config,
        client: api.ViewerClient,
        model: torch.nn.Module,
        device: torch.device,
    ):
        self.cfg = cfg
        self.api = client
        self.model = model
        self.device = device
        self.micro_batch = cfg.micro_batch or (32 if device.type == "cuda" else 16)
        self.download_client = httpx.Client(
            timeout=cfg.timeout, follow_redirects=True
        )

        # Bounded hand-off to the embed thread: preprocessed pixels are
        # ~580 KiB each, so the cap keeps memory flat under backpressure.
        self._pixels: queue.Queue = queue.Queue(maxsize=max(self.micro_batch * 4, 16))

        # Shared between the main and embed threads; guarded by _state_lock.
        self._state_lock = threading.Lock()
        self._results: list[dict] = []
        self._last_flush = time.monotonic()
        self._in_flight: set[str] = set()
        self._lease_until: datetime | None = None
        self._counts = {"embedded": 0, "failed": 0, "skipped": 0}
        self._bytes_total = 0  # claimed blob bytes, for the average-throughput log
        self._started = time.monotonic()

        # Only one POST may be in flight at a time; taken without blocking so
        # a slow upload never stalls the other thread's forward passes.
        self._post_lock = threading.Lock()

        self._embed_thread: threading.Thread | None = None
        self._embed_error: Exception | None = None
        self._embed_dead = threading.Event()

    def run(self, stop: threading.Event, once: bool) -> None:
        """Claim/download loop; exits on stop, after one cycle with once, or fatally."""
        self._start_embed_thread()
        backoff = 1.0
        try:
            while not stop.is_set():
                self._check_embed_alive()
                try:
                    response = self.api.claim(self.cfg.claim_limit)
                except api.AuthError:
                    raise
                except api.TransientError as exc:
                    log.warning("claim failed (%s); retrying in %.1fs", exc, backoff)
                    self._upkeep_wait(stop, backoff * random.uniform(0.5, 1.5))
                    backoff = min(backoff * 2, 60.0)
                    continue
                backoff = 1.0

                if response.embedding_dim != EMBEDDING_DIM:
                    log.critical(
                        "server expects %d-d embeddings but this worker provides %d-d; "
                        "wrong model for this server?",
                        response.embedding_dim,
                        EMBEDDING_DIM,
                    )
                    raise SystemExit(1)

                if not response.claimed:
                    if once:
                        log.info("nothing pending")
                        return
                    nap = self.cfg.poll_interval * random.uniform(0.8, 1.2)
                    log.debug("nothing claimed; sleeping %.1fs", nap)
                    self._upkeep_wait(stop, nap)
                    continue

                self._process_batch(response, stop)
                self._flush_if_due()
                if once:
                    return
        finally:
            self._shutdown_embed()

    # ------------------------------------------------------------------
    # Batch consumption (main thread)
    # ------------------------------------------------------------------

    def _process_batch(self, response: api.ClaimResponse, stop: threading.Event) -> None:
        items = response.claimed
        total_bytes = sum(blob.size_bytes for blob in items)
        self._bytes_total += total_bytes
        with self._state_lock:
            fresh = not self._in_flight
            self._in_flight.update(blob.hash for blob in items)
            if fresh or self._lease_until is None:
                self._lease_until = response.lease_until
            else:
                # Track the earliest expiry; renew covers every live blob.
                self._lease_until = min(self._lease_until, response.lease_until)
        remaining = (response.lease_until - datetime.now(timezone.utc)).total_seconds()
        log.info(
            "claimed %d blobs (%.1f MiB, lease until %s, %ds)",
            len(items),
            total_bytes / (1024 * 1024),
            response.lease_until.astimezone().strftime("%H:%M:%S"),
            max(int(remaining), 0),
        )

        # Every presigned URL lives ~15 min, so submit all downloads up front
        # and consume their results in submission order.
        pool = ThreadPoolExecutor(max_workers=self.cfg.download_workers)
        downloads = [
            (blob, pool.submit(self._download_one, blob)) for blob in items
        ]

        failed = skipped = 0
        for blob, future in downloads:
            if stop.is_set():
                break
            try:
                # Wait in slices so lease renewal keeps running while the
                # next download trickles in.
                while True:
                    try:
                        pixels = future.result(timeout=ENQUEUE_SLICE_SECONDS)
                        break
                    except FutureTimeoutError:
                        self._renew_if_due()
            except PermanentError as exc:
                self._add_results([_failed_result(blob.hash, exc)])
                failed += 1
                continue
            except (SkipTransient, Exception) as exc:
                # SkipTransient by design; anything unexpected is treated the
                # same way: omit the blob, its lease expires and it retries.
                self._skip_hashes([blob.hash])
                log.info("skipping %s (%s)", blob.hash[:12], _error_text(exc))
                skipped += 1
                continue

            if self._enqueue(blob.hash, pixels, stop):
                continue
            skipped += 1  # stopped mid-batch; the lease expires and it retries

        self._flush_if_due()
        pool.shutdown(wait=False, cancel_futures=True)
        with self._state_lock:
            embedded_total = self._counts["embedded"]
        elapsed = time.monotonic() - self._started
        rate = embedded_total / elapsed if elapsed > 0 else 0.0
        mib_s = self._bytes_total / elapsed / (1024 * 1024) if elapsed > 0 else 0.0
        log.info(
            "batch done: claimed=%d failed=%d skipped=%d "
            "(embedded total=%d, %.1f img/s avg, %.1f MiB/s avg)",
            len(items),
            failed,
            skipped,
            embedded_total,
            rate,
            mib_s,
        )

    def _enqueue(self, blob_hash: str, pixels: np.ndarray, stop: threading.Event) -> bool:
        """Hand preprocessed pixels to the embed thread; False if stopped."""
        while True:
            self._check_embed_alive()
            if stop.is_set():
                self._skip_hashes([blob_hash])
                return False
            try:
                self._pixels.put((blob_hash, pixels), timeout=ENQUEUE_SLICE_SECONDS)
                return True
            except queue.Full:
                # Backpressure: the GPU is behind, keep the leases alive.
                self._renew_if_due()

    def _download_one(self, blob: api.ClaimedBlob) -> np.ndarray:
        return download_and_preprocess(self.download_client, blob.hash, blob.get_url)

    def _upkeep_wait(self, stop: threading.Event, seconds: float) -> None:
        """Interruptible sleep that keeps results flushing and leases renewing."""
        deadline = time.monotonic() + seconds
        while not stop.is_set():
            self._flush_if_due()
            self._renew_if_due()
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return
            stop.wait(min(remaining, WAIT_SLICE_SECONDS))

    # ------------------------------------------------------------------
    # Embedding (embed thread)
    # ------------------------------------------------------------------

    def _start_embed_thread(self) -> None:
        self._embed_thread = threading.Thread(
            target=self._embed_loop, name="embed", daemon=True
        )
        self._embed_thread.start()

    def _embed_loop(self) -> None:
        buffer: list[tuple[str, np.ndarray]] = []
        try:
            while True:
                try:
                    item = self._pixels.get(timeout=WAIT_SLICE_SECONDS)
                except queue.Empty:
                    # Idle: results may still be waiting on the flush timer.
                    self._flush_if_due()
                    continue
                if item is None:
                    break
                buffer.append(item)
                if len(buffer) >= self.micro_batch:
                    self._embed(buffer)
                    buffer = []
                    self._flush_if_due()
            if buffer:
                self._embed(buffer)
        except Exception as exc:  # unexpected; surface as fatal to the main loop
            self._embed_error = exc
            self._embed_dead.set()
            log.critical("embedding thread crashed: %s", _error_text(exc))

    def _embed(self, buffer: list[tuple[str, np.ndarray]]) -> None:
        """Run the model over (hash, pixels) pairs; append ready/failed results."""
        remaining = buffer
        while remaining:
            chunk, remaining = (
                remaining[: self.micro_batch],
                remaining[self.micro_batch :],
            )
            try:
                output = self._forward([pixels for _, pixels in chunk])
            except RuntimeError as exc:
                if not _is_oom(exc):
                    self._skip_hashes([blob_hash for blob_hash, _ in chunk])
                    log.warning(
                        "forward pass failed; skipping %d blobs: %s",
                        len(chunk),
                        _error_text(exc),
                    )
                    continue
                if torch.cuda.is_available():
                    torch.cuda.empty_cache()
                if self.micro_batch > 1:
                    self.micro_batch = max(1, self.micro_batch // 2)
                    log.warning(
                        "out of memory; halved micro-batch to %d", self.micro_batch
                    )
                    remaining = chunk + remaining  # re-queue at the smaller size
                    continue
                log.warning("out of memory at micro-batch 1; skipping %d blobs", len(chunk))
                self._skip_hashes([blob_hash for blob_hash, _ in chunk])
                continue

            ready: list[dict] = []
            failed: list[dict] = []
            for (blob_hash, _), row in zip(chunk, output):
                if np.isfinite(row).all():
                    ready.append(
                        {
                            "hash": blob_hash,
                            "status": "ready",
                            "vectorB64": vectors.encode(row),
                            "error": "",
                        }
                    )
                else:
                    # Deterministic per input, so terminal failed — this also
                    # keeps the server from ever seeing a bad_vector rejection.
                    failed.append(
                        _failed_result(blob_hash, "model produced a non-finite vector")
                    )
            # Hashes stay in flight until their results are POSTed, so a slow
            # flush can never let a finished blob's lease lapse and get the
            # same image re-claimed and re-embedded by another worker.
            self._add_results(ready + failed)

    def _forward(self, pixels: list[np.ndarray]) -> np.ndarray:
        tensors = torch.from_numpy(np.stack(pixels)).to(self.device)
        with torch.no_grad():
            output = self.model(tensors)
        return output.float().cpu().numpy()

    # ------------------------------------------------------------------
    # Shared state: results buffer, in-flight leases
    # ------------------------------------------------------------------

    def _add_results(self, results: list[dict]) -> None:
        with self._state_lock:
            self._results.extend(results)
            for item in results:
                if item["status"] == "ready":
                    self._counts["embedded"] += 1
                else:
                    self._counts["failed"] += 1
        if results:
            self._flush_if_due()

    def _skip_hashes(self, blob_hashes: list[str]) -> None:
        """Drop blobs with no result: their lease expires and they retry."""
        with self._state_lock:
            for blob_hash in blob_hashes:
                self._in_flight.discard(blob_hash)
            self._counts["skipped"] += len(blob_hashes)

    def _flush_if_due(self, force: bool = False) -> None:
        """POST accumulated results when the chunk size, timer, or shutdown says so."""
        if not self._post_lock.acquire(blocking=False):
            return  # another flush is already uploading
        try:
            while True:
                with self._state_lock:
                    age = time.monotonic() - self._last_flush
                    due = (
                        force
                        or len(self._results) >= RESULTS_CHUNK
                        or (bool(self._results) and age >= RESULTS_FLUSH_SECONDS)
                    )
                    if not due or not self._results:
                        return
                    chunk = self._results[:RESULTS_CHUNK]
                    del self._results[:RESULTS_CHUNK]
                if not self._post_results(chunk):
                    # Server unreachable; retry the same results after the interval.
                    with self._state_lock:
                        self._results[:0] = chunk
                        self._last_flush = time.monotonic()
                    log.warning(
                        "results upload failed; requeued %d results for retry",
                        len(chunk),
                    )
                    return
                with self._state_lock:
                    for item in chunk:
                        self._in_flight.discard(item["hash"])
                    self._last_flush = time.monotonic()
        finally:
            self._post_lock.release()

    def _renew_if_due(self) -> None:
        """Renew the leases of all live blobs shortly before they lapse."""
        with self._state_lock:
            if not self._in_flight or self._lease_until is None:
                return
            remaining = (self._lease_until - datetime.now(timezone.utc)).total_seconds()
            if remaining > LEASE_RENEW_MARGIN_SECONDS:
                return
            hashes = sorted(self._in_flight)
        try:
            response = self.api.renew(hashes)
        except api.TransientError as exc:
            log.warning("lease renew failed (%s); retrying soon", exc)
            return
        renewed = response.renewed
        with self._state_lock:
            # A blob posted while the renew was in flight is done, not lost.
            lost = [
                blob_hash
                for blob_hash in hashes
                if blob_hash not in renewed and blob_hash in self._in_flight
            ]
            for blob_hash in lost:
                self._in_flight.discard(blob_hash)
            self._lease_until = response.lease_until
        for blob_hash in lost:
            log.warning("lost lease on %s (expired or claimed elsewhere)", blob_hash[:12])
        log.info(
            "renewed %d/%d leases until %s",
            len(renewed),
            len(hashes),
            response.lease_until.astimezone().strftime("%H:%M:%S"),
        )

    def _check_embed_alive(self) -> None:
        if self._embed_dead.is_set():
            # AuthError keeps its own fatal path: the CLI turns it into exit 2.
            if isinstance(self._embed_error, api.AuthError):
                raise self._embed_error
            raise RuntimeError(f"embedding thread died: {self._embed_error}")

    def _shutdown_embed(self) -> None:
        """Drain the queue, flush remaining results, and stop the embed thread."""
        if self._embed_thread is None:
            return
        while not self._embed_dead.is_set():
            try:
                self._pixels.put(None, timeout=WAIT_SLICE_SECONDS)
                break
            except queue.Full:
                continue
        self._embed_thread.join()
        self._flush_if_due(force=True)

    # ------------------------------------------------------------------
    # Result upload
    # ------------------------------------------------------------------

    def _post_results(self, results: list[dict]) -> bool:
        """POST embedded vectors in chunks; rejected items are logged, never retried.

        Returns False when the server was unreachable and the results must be
        retried later.
        """
        for index in range(0, len(results), RESULTS_CHUNK):
            chunk = results[index : index + RESULTS_CHUNK]
            try:
                response = self._post_results_chunk(chunk)
            except api.BadRequestError as exc:
                if "too many results" in str(exc) and len(chunk) > 1:
                    mid = len(chunk) // 2
                    if not self._post_results(chunk[:mid]):
                        return False
                    if not self._post_results(chunk[mid:]):
                        return False
                else:
                    log.error("results chunk rejected by server: %s", exc)
                continue
            except api.TransientError:
                return False
            log.info("results: updated=%d rejected=%d", response.updated, len(response.rejected))
            for rejection in response.rejected:
                log.warning("rejected %s: %s", rejection.hash[:12], rejection.reason)
        return True

    def _post_results_chunk(self, chunk: list[dict]) -> api.ResultsResponse:
        delay = 1.0
        for attempt in range(5):
            try:
                return self.api.results(chunk)
            except api.TransientError as exc:
                if attempt == 4:
                    raise
                log.warning("results post failed (%s); retrying in %.1fs", exc, delay)
                time.sleep(delay)
                delay = min(delay * 2, 15.0)
        raise AssertionError("unreachable")


def _failed_result(blob_hash: str, exc: object) -> dict:
    return {
        "hash": blob_hash,
        "status": "failed",
        "vectorB64": "",
        "error": _error_text(exc),
    }


def _error_text(exc: object) -> str:
    return " ".join(str(exc).split())[:ERROR_TEXT_LIMIT] or exc.__class__.__name__


def _is_oom(exc: RuntimeError) -> bool:
    return isinstance(exc, torch.cuda.OutOfMemoryError) or "out of memory" in str(exc).lower()
