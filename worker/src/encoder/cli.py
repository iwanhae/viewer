"""Continuous WebP Q85 worker using the official cwebp CLI."""

from __future__ import annotations

import argparse
import hashlib
import logging
import os
from pathlib import Path
import random
import shutil
import signal
import subprocess
import tempfile
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

import httpx

from worker_common import env
from worker_common.http import AuthError, BadRequestError, TransientError, ViewerClient

log = logging.getLogger(__name__)


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description="Continuous WebP Q85 encoding worker")
    p.add_argument("--base-url", default=os.getenv("VIEWER_BASE_URL", ""))
    p.add_argument("--token", default=os.getenv("WORKER_TOKEN", ""))
    p.add_argument("--workers", type=int, default=int(os.getenv("ENCODER_WORKERS", "8")))
    p.add_argument("--poll-interval", type=float, default=float(os.getenv("POLL_INTERVAL", "10")))
    p.add_argument("--timeout", type=float, default=float(os.getenv("HTTP_TIMEOUT", "60")))
    return p


class EncoderClient(ViewerClient):
    def claim(self, limit: int) -> list[dict]:
        return self._post("/api/encoding/claim", {"limit": limit}).json()["claimed"]

    def renew(self, job: dict) -> str:
        result = self._post("/api/encoding/renew", {"hash": job["hash"], "token": job["token"]}).json()
        return result["putUrl"]

    def complete(self, job: dict, outcome: str, error: str = "") -> str:
        result = self._post("/api/encoding/complete", {
            "hash": job["hash"], "token": job["token"], "outcome": outcome, "error": error[:512],
        }).json()
        return result["status"]


def _download(client: httpx.Client, url: str, path: Path) -> int:
    count = 0
    with client.stream("GET", url) as response:
        response.raise_for_status()
        with path.open("wb") as out:
            for chunk in response.iter_bytes():
                count += len(chunk)
                out.write(chunk)
    return count


def _signature(path: Path) -> str:
    with path.open("rb") as source:
        header = source.read(12)
    if len(header) >= 12 and header[:4] == b"RIFF" and header[8:12] == b"WEBP":
        return "webp"
    if header.startswith(b"\xff\xd8\xff"):
        return "jpeg"
    if header.startswith(b"\x89PNG\r\n\x1a\n"):
        return "png"
    return "unsupported"


def _renew_loop(api: EncoderClient, job: dict, stop: threading.Event, put_url: list[str]) -> None:
    while not stop.wait(120):
        try:
            put_url[0] = api.renew(job)
        except (TransientError, BadRequestError) as exc:
            log.warning("renew %s: %s", job["hash"][:12], exc)
            if isinstance(exc, BadRequestError):
                return


def _process_one(api: EncoderClient, job: dict, timeout: float) -> tuple[str, int, int]:
    hash_ = job["hash"]
    started = time.monotonic()
    put_url = [job["putUrl"]]
    stop_renew = threading.Event()
    renewer = threading.Thread(target=_renew_loop, args=(api, job, stop_renew, put_url), daemon=True)
    renewer.start()
    try:
        with tempfile.TemporaryDirectory(prefix="encoder-") as directory, httpx.Client(timeout=timeout, follow_redirects=True) as transfer:
            source = Path(directory) / "source"
            output = Path(directory) / "output.webp"
            original_bytes = _download(transfer, job["getUrl"], source)
            format_ = _signature(source)
            if format_ == "webp":
                api.complete(job, "already_webp")
                return "already_webp", original_bytes, original_bytes
            if format_ not in ("jpeg", "png"):
                api.complete(job, "failed", "unsupported input format")
                return "failed", original_bytes, original_bytes
            with source.open("rb") as body:
                digest = hashlib.file_digest(body, "sha256").hexdigest()
            if digest != hash_:
                log.warning("source hash changed for %s", hash_[:12])
                return "retry", original_bytes, original_bytes
            try:
                subprocess.run(
                    ["cwebp", "-q", "85", "-alpha_q", "100", "-metadata", "all", str(source), "-o", str(output)],
                    check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=360,
                )
            except subprocess.TimeoutExpired:
                log.warning("cwebp timed out for %s", hash_[:12])
                return "retry", original_bytes, original_bytes
            except subprocess.CalledProcessError as exc:
                api.complete(job, "failed", exc.stderr.decode("utf-8", "replace")[-300:])
                return "failed", original_bytes, original_bytes
            encoded_bytes = output.stat().st_size
            if encoded_bytes >= original_bytes:
                api.complete(job, "not_smaller")
                return "not_smaller", original_bytes, original_bytes
            with output.open("rb") as body:
                response = transfer.put(put_url[0], content=body, headers={"Content-Type": "image/webp"})
                response.raise_for_status()
            api.complete(job, "uploaded")
            log.info("encoded %s: %d -> %d bytes (%.1f%% saved) in %.1fs", hash_[:12], original_bytes,
                     encoded_bytes, 100 * (original_bytes - encoded_bytes) / original_bytes, time.monotonic() - started)
            return "done", original_bytes, encoded_bytes
    except (httpx.HTTPError, TransientError, BadRequestError, OSError) as exc:
        log.warning("encoding %s will retry after lease expiry: %s", hash_[:12], exc)
        return "retry", 0, 0
    finally:
        stop_renew.set()
        renewer.join(timeout=1)


def main(argv=None) -> int:
    loaded = env.load_dotenv()
    args = parser().parse_args(argv)
    if not args.base_url or not args.token:
        raise SystemExit("VIEWER_BASE_URL and WORKER_TOKEN are required")
    if not 1 <= args.workers <= 64 or args.poll_interval <= 0:
        raise SystemExit("--workers must be 1..64 and --poll-interval must be positive")
    if shutil.which("cwebp") is None:
        raise SystemExit("cwebp is required; install the official WebP CLI")
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)-7s %(message)s")
    if loaded:
        log.info("loaded environment from %s", loaded)
    stop = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stop.set())
    api = EncoderClient(args.base_url.rstrip("/"), args.token, args.timeout)
    backoff = 1.0
    while not stop.is_set():
        try:
            jobs = api.claim(args.workers)
        except AuthError as exc:
            log.error("%s; check WORKER_TOKEN", exc)
            return 2
        except TransientError as exc:
            log.warning("claim failed: %s", exc)
            stop.wait(backoff * random.uniform(0.8, 1.2))
            backoff = min(backoff * 2, 60)
            continue
        backoff = 1.0
        if not jobs:
            stop.wait(args.poll_interval)
            continue
        started = time.monotonic()
        saved = 0
        counts: dict[str, int] = {}
        with ThreadPoolExecutor(max_workers=args.workers) as pool:
            futures = [pool.submit(_process_one, api, job, args.timeout) for job in jobs]
            for future in as_completed(futures):
                outcome, before, after = future.result()
                counts[outcome] = counts.get(outcome, 0) + 1
                saved += before - after
        elapsed = time.monotonic() - started
        log.info("batch: %d images in %.1fs (%.2f/s), saved %.2f MiB, outcomes=%s",
                 len(jobs), elapsed, len(jobs) / elapsed, saved / (1024 * 1024), counts)
    return 0
