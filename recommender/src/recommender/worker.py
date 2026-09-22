"""The embedding loop: claim -> download -> embed -> renew -> results."""

from __future__ import annotations

import logging
import random
import threading
import time
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
ERROR_TEXT_LIMIT = 200  # the server keeps 512 bytes of error text; stay short


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

    def run(self, stop: threading.Event, once: bool) -> None:
        """Claim/embed loop; exits on stop, after one cycle with once, or fatally."""
        backoff = 1.0
        while not stop.is_set():
            try:
                response = self.api.claim(self.cfg.claim_limit)
            except api.AuthError:
                raise
            except api.TransientError as exc:
                log.warning("claim failed (%s); retrying in %.1fs", exc, backoff)
                stop.wait(backoff * random.uniform(0.5, 1.5))
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
                stop.wait(nap)
                continue

            self._process_batch(response, stop)
            if once:
                return

    def _process_batch(self, response: api.ClaimResponse, stop: threading.Event) -> None:
        items = response.claimed
        lease_until = response.lease_until
        remaining = (lease_until - datetime.now(timezone.utc)).total_seconds()
        log.info(
            "claimed %d blobs (lease until %s, %ds)",
            len(items),
            lease_until.astimezone().strftime("%H:%M:%S"),
            max(int(remaining), 0),
        )

        # Every presigned URL lives ~15 min, so submit all downloads up front
        # and consume their results in submission order.
        pool = ThreadPoolExecutor(max_workers=self.cfg.download_workers)
        downloads = [
            (blob, pool.submit(self._download_one, blob)) for blob in items
        ]
        in_flight = {blob.hash for blob, _ in downloads}

        counts = {"embedded": 0, "failed": 0, "skipped": 0}
        results: list[dict] = []
        buffer: list[tuple[str, np.ndarray]] = []
        started = time.monotonic()

        for blob, future in downloads:
            if stop.is_set():
                break
            try:
                pixels = future.result()
            except PermanentError as exc:
                in_flight.discard(blob.hash)
                results.append(_failed_result(blob.hash, exc))
                counts["failed"] += 1
                continue
            except (SkipTransient, Exception) as exc:
                # SkipTransient by design; anything unexpected is treated the
                # same way: omit the blob, its lease expires and it retries.
                in_flight.discard(blob.hash)
                log.info("skipping %s (%s)", blob.hash[:12], _error_text(exc))
                counts["skipped"] += 1
                continue

            buffer.append((blob.hash, pixels))
            if len(buffer) >= self.micro_batch:
                self._embed(buffer, results, in_flight, counts)
                lease_until = self._renew_if_due(lease_until, in_flight)
                buffer = []

        if buffer and not stop.is_set():
            self._embed(buffer, results, in_flight, counts)
        self._post_results(results)

        pool.shutdown(wait=False, cancel_futures=True)
        elapsed = time.monotonic() - started
        rate = counts["embedded"] / elapsed if elapsed > 0 else 0.0
        log.info(
            "batch done: claimed=%d embedded=%d failed=%d skipped=%d in %.1fs (%.1f img/s)",
            len(items),
            counts["embedded"],
            counts["failed"],
            counts["skipped"],
            elapsed,
            rate,
        )

    def _download_one(self, blob: api.ClaimedBlob) -> np.ndarray:
        return download_and_preprocess(self.download_client, blob.hash, blob.get_url)

    def _embed(
        self,
        buffer: list[tuple[str, np.ndarray]],
        results: list[dict],
        in_flight: set[str],
        counts: dict[str, int],
    ) -> None:
        """Run the model over (hash, pixels) pairs; append ready/failed results."""
        remaining = list(buffer)
        while remaining:
            chunk, remaining = (
                remaining[: self.micro_batch],
                remaining[self.micro_batch :],
            )
            try:
                output = self._forward([pixels for _, pixels in chunk])
            except RuntimeError as exc:
                if not _is_oom(exc):
                    for blob_hash, _ in chunk:
                        in_flight.discard(blob_hash)
                        counts["skipped"] += 1
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
                for blob_hash, _ in chunk:
                    in_flight.discard(blob_hash)
                    counts["skipped"] += 1
                continue

            for (blob_hash, _), row in zip(chunk, output):
                in_flight.discard(blob_hash)
                if np.isfinite(row).all():
                    results.append(
                        {
                            "hash": blob_hash,
                            "status": "ready",
                            "vectorB64": vectors.encode(row),
                            "error": "",
                        }
                    )
                    counts["embedded"] += 1
                else:
                    # Deterministic per input, so terminal failed — this also
                    # keeps the server from ever seeing a bad_vector rejection.
                    results.append(_failed_result(blob_hash, "model produced a non-finite vector"))
                    counts["failed"] += 1

    def _forward(self, pixels: list[np.ndarray]) -> np.ndarray:
        tensors = torch.from_numpy(np.stack(pixels)).to(self.device)
        with torch.no_grad():
            output = self.model(tensors)
        return output.float().cpu().numpy()

    def _renew_if_due(self, lease_until: datetime, in_flight: set[str]) -> datetime:
        """Renew the batch lease shortly before it lapses; only for live blobs."""
        remaining = (lease_until - datetime.now(timezone.utc)).total_seconds()
        if not in_flight or remaining > LEASE_RENEW_MARGIN_SECONDS:
            return lease_until
        hashes = sorted(in_flight)
        try:
            response = self.api.renew(hashes)
        except api.TransientError as exc:
            log.warning("lease renew failed (%s); retrying after the next chunk", exc)
            return lease_until
        renewed = set(response.renewed)
        for blob_hash in hashes:
            if blob_hash not in renewed:
                in_flight.discard(blob_hash)
                log.warning("lost lease on %s (expired or claimed elsewhere)", blob_hash[:12])
        log.info(
            "renewed %d/%d leases until %s",
            len(response.renewed),
            len(hashes),
            response.lease_until.astimezone().strftime("%H:%M:%S"),
        )
        return response.lease_until

    def _post_results(self, results: list[dict]) -> None:
        """POST embedded vectors in chunks; rejected items are logged, never retried."""
        for index in range(0, len(results), RESULTS_CHUNK):
            chunk = results[index : index + RESULTS_CHUNK]
            try:
                response = self._post_results_chunk(chunk)
            except api.BadRequestError as exc:
                if "too many results" in str(exc) and len(chunk) > 1:
                    mid = len(chunk) // 2
                    self._post_results(chunk[:mid])
                    self._post_results(chunk[mid:])
                else:
                    log.error("results chunk rejected by server: %s", exc)
                continue
            log.info("results: updated=%d rejected=%d", response.updated, len(response.rejected))
            for rejection in response.rejected:
                log.warning("rejected %s: %s", rejection.hash[:12], rejection.reason)

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
