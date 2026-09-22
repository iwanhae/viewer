"""Worker configuration: argparse flags override env vars override defaults."""

from __future__ import annotations

import argparse
import os
from dataclasses import dataclass
from pathlib import Path

DEFAULT_MODEL_URL = "https://s3.iwanhae.kr/public/google/siglip2-base-patch16-224"

# Server-side constants this client must respect (internal/recommend/types.go).
EMBEDDING_DIM = 768
MAX_CLAIM_LIMIT = 1024
LEASE_RENEW_MARGIN_SECONDS = 180  # renew while at least this much of the 10-min lease remains


@dataclass(frozen=True)
class Config:
    base_url: str
    token: str
    model_url: str
    model_path: Path | None
    claim_limit: int
    micro_batch: int  # images per forward pass; 0 = auto by device
    poll_interval: float
    download_workers: int
    device: str  # auto | cpu | cuda
    model_cache_dir: Path
    timeout: float
    log_level: str
    once: bool


def _env(name: str, default: str = "") -> str:
    return os.environ.get(name, default)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="recommender",
        description="External embedding worker for the viewer photo server.",
    )
    parser.add_argument(
        "--base-url",
        default=_env("VIEWER_BASE_URL"),
        help="viewer base URL, e.g. https://albums.iwanhae.kr (VIEWER_BASE_URL)",
    )
    parser.add_argument(
        "--token",
        default=_env("VIEWER_WORKER_TOKEN") or _env("EMBEDDING_WORKER_TOKEN"),
        help="worker bearer token (VIEWER_WORKER_TOKEN, falls back to EMBEDDING_WORKER_TOKEN)",
    )
    parser.add_argument(
        "--model-url",
        default=_env("SIGLIP2_MODEL_URL", DEFAULT_MODEL_URL),
        help="checkpoint mirror serving config.json + model.safetensors (SIGLIP2_MODEL_URL)",
    )
    parser.add_argument(
        "--model",
        default=_env("SIGLIP2_MODEL"),
        help="local model.safetensors path; skips the download (SIGLIP2_MODEL)",
    )
    parser.add_argument(
        "--claim-limit",
        type=int,
        default=int(_env("CLAIM_LIMIT", "64")),
        help="blobs per claim, 1..1024 (CLAIM_LIMIT)",
    )
    parser.add_argument(
        "--micro-batch",
        type=int,
        default=int(_env("MICRO_BATCH", "0")),
        help="images per forward pass; 0 = auto, 32 on cuda / 16 on cpu (MICRO_BATCH)",
    )
    parser.add_argument(
        "--poll-interval",
        type=float,
        default=float(_env("POLL_INTERVAL", "20")),
        help="seconds to sleep when nothing is pending (POLL_INTERVAL)",
    )
    parser.add_argument(
        "--download-workers",
        type=int,
        default=int(_env("DOWNLOAD_WORKERS", "8")),
        help="concurrent presigned-GET downloads (DOWNLOAD_WORKERS)",
    )
    parser.add_argument(
        "--device",
        default=_env("DEVICE", "auto"),
        choices=["auto", "cpu", "cuda"],
        help="torch device (DEVICE)",
    )
    parser.add_argument(
        "--model-cache-dir",
        default=_env("MODEL_CACHE_DIR"),
        help="checkpoint cache directory (MODEL_CACHE_DIR, default <recommender>/.cache/models)",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=float(_env("HTTP_TIMEOUT", "60")),
        help="HTTP timeout in seconds (HTTP_TIMEOUT)",
    )
    parser.add_argument(
        "--log-level",
        default=_env("LOG_LEVEL", "INFO"),
        help="log level (LOG_LEVEL)",
    )
    parser.add_argument(
        "--once",
        action="store_true",
        help="run a single claim -> embed -> results cycle and exit",
    )
    return parser


def parse(argv=None) -> Config:
    args = build_parser().parse_args(argv)
    if not args.base_url:
        raise SystemExit(
            "VIEWER_BASE_URL is required (--base-url or the VIEWER_BASE_URL env var)"
        )

    model_path = Path(args.model).resolve() if args.model else None
    if model_path is not None and not model_path.is_file():
        raise SystemExit(f"model file not found: {model_path}")

    cache_dir = (
        Path(args.model_cache_dir)
        if args.model_cache_dir
        else Path(__file__).resolve().parents[2] / ".cache" / "models"
    )

    return Config(
        base_url=args.base_url.rstrip("/"),
        token=args.token.strip(),
        model_url=args.model_url.rstrip("/"),
        model_path=model_path,
        claim_limit=min(max(args.claim_limit, 1), MAX_CLAIM_LIMIT),
        micro_batch=max(args.micro_batch, 0),
        poll_interval=max(args.poll_interval, 1.0),
        download_workers=min(max(args.download_workers, 1), 32),
        device=args.device,
        model_cache_dir=cache_dir,
        timeout=max(args.timeout, 1.0),
        log_level=args.log_level,
        once=args.once,
    )
