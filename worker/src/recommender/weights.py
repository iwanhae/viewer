"""Fetch the SigLIP2 checkpoint into a local cache (once), or use a local file."""

from __future__ import annotations

import logging
import os
from pathlib import Path

import httpx

from .config import Config

log = logging.getLogger(__name__)

FILES = ("config.json", "model.safetensors")


def ensure(cfg: Config) -> Path:
    """Return a path to model.safetensors, downloading it first if needed."""
    if cfg.model_path is not None:
        log.info("using local checkpoint %s", cfg.model_path)
        return cfg.model_path

    target = cfg.model_cache_dir / "model.safetensors"
    if target.is_file() and target.stat().st_size > 0:
        log.info("checkpoint cache hit: %s", target)
        return target

    cfg.model_cache_dir.mkdir(parents=True, exist_ok=True)
    try:
        with httpx.Client(timeout=cfg.timeout, follow_redirects=True) as client:
            for name in FILES:
                _download(client, f"{cfg.model_url}/{name}", cfg.model_cache_dir / name)
    except httpx.HTTPError as exc:
        raise SystemExit(f"could not download checkpoint from {cfg.model_url}: {exc}") from exc
    return target


def _download(client: httpx.Client, url: str, dest: Path) -> None:
    if dest.is_file() and dest.stat().st_size > 0:
        return
    partial = dest.with_name(f"{dest.name}.part-{os.getpid()}")
    log.info("downloading %s", url)
    with client.stream("GET", url) as response:
        response.raise_for_status()
        with partial.open("wb") as handle:
            for chunk in response.iter_bytes():
                handle.write(chunk)
    os.replace(partial, dest)  # atomic: never leave a half-written checkpoint behind
