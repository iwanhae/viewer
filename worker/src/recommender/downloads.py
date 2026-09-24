"""Concurrent blob download + decode, with the worker's two failure classes.

A SkipTransient means "try again later": the blob is omitted from the
results so its lease simply expires (within 10 minutes) and the next claim
retries it. A PermanentError means the blob can never embed and is reported
to the server as a terminal failed result.
"""

from __future__ import annotations

import httpx
import numpy as np
from PIL import Image

from .preprocess import preprocess


class SkipTransient(Exception):
    """Retry later by omission: let the lease expire and get re-claimed."""


class PermanentError(Exception):
    """Deterministic failure; report to the server as status=failed."""


def download_and_preprocess(client: httpx.Client, blob_hash: str, url: str) -> np.ndarray:
    """Presigned GET + Pillow decode + preprocess, on a worker thread."""
    try:
        response = client.get(url)
    except httpx.TransportError as exc:
        raise SkipTransient(f"download failed: {exc}") from exc
    if response.status_code != 200:
        # Presign expiry shows up as 403; any miss is retried after the
        # lease lapses rather than burned as a permanent failure.
        raise SkipTransient(f"download status {response.status_code}")
    if not response.content:
        raise SkipTransient("empty body")
    try:
        return preprocess(response.content)
    except Image.DecompressionBombError as exc:
        raise PermanentError(f"image too large to decode: {exc}") from exc
    except (OSError, ValueError, ZeroDivisionError) as exc:
        # UnidentifiedImageError and truncated-file errors are OSError
        # subclasses; a blob that can never decode is a permanent failure.
        raise PermanentError(f"cannot decode image: {exc}") from exc
