"""Minimal .env loader.

Loads the first .env found — the current working directory, then the
worker/ directory — into os.environ without ever overriding variables
that are already set, so real environment variables always win.
"""

from __future__ import annotations

import logging
import os
from pathlib import Path

log = logging.getLogger(__name__)


def load_dotenv() -> Path | None:
    """Apply the first .env found and return its path, or None."""
    here = Path(__file__).resolve().parents[2]  # .../worker
    for candidate in (Path.cwd() / ".env", here / ".env"):
        if candidate.is_file():
            _apply(candidate)
            return candidate
    return None


def _apply(path: Path) -> None:
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        key = key.strip()
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        if key and key not in os.environ:
            os.environ[key] = value
