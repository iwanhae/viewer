"""Command-line entry point for the embedding worker."""

from __future__ import annotations

import logging
import signal
import threading

from . import api, config, weights
from worker_common import env

log = logging.getLogger(__name__)


def main(argv=None) -> int:
    loaded = env.load_dotenv()
    cfg = config.parse(argv)
    logging.basicConfig(
        format="%(asctime)s %(levelname)-7s %(message)s",
        datefmt="%H:%M:%S",
        level=getattr(logging, cfg.log_level.upper(), logging.INFO),
    )
    if loaded is not None:
        log.info("loaded environment from %s", loaded)
    logging.getLogger("httpx").setLevel(logging.WARNING)  # presigned URLs are noisy at INFO
    if not cfg.token:
        log.warning(
            "no worker token set; the server will 401 unless it runs without "
            "WORKER_TOKEN"
        )

    try:
        import torch
    except ImportError:
        log.error(
            "torch is not installed; run: uv sync --extra cpu "
            "(or --extra cu128 on a CUDA box)"
        )
        return 1

    from .model import load_model
    from .worker import Worker

    device = _resolve_device(cfg.device)
    if device.type == "cuda":
        log.info("device: %s (%s)", device, torch.cuda.get_device_name())
    else:
        log.info("device: %s", device)

    checkpoint = weights.ensure(cfg)
    model = load_model(checkpoint, device)

    stop = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stop.set())

    client = api.ViewerClient(cfg.base_url, cfg.token, cfg.timeout)
    try:
        Worker(cfg, client, model, device).run(stop, once=cfg.once)
    except api.AuthError as exc:
        log.critical("%s; check WORKER_TOKEN", exc)
        return 2
    return 0


def _resolve_device(preference: str):
    import torch

    if preference == "cpu":
        return torch.device("cpu")
    if preference == "cuda":
        if not torch.cuda.is_available():
            raise SystemExit("--device cuda requested, but CUDA is not available")
        return torch.device("cuda")
    return torch.device("cuda" if torch.cuda.is_available() else "cpu")
