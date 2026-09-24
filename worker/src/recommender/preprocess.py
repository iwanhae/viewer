"""Image preprocessing for the SigLIP2 vision tower.

Adapted from scripts/vision-reference/generate.py (resize_to_fill), the
reference the Go server's internal/vision preprocessor is regression-tested
against: aspect-preserving resize so the short side covers the target, then
a centered crop, then rescale to [-1, 1] with x/127.5 - 1 (SigLIP's
mean=std=0.5).

The Go decoder works in RGBA before resizing while Pillow decodes straight
to RGB here; identical for opaque JPEG, and a transparent PNG may differ by
a few least-significant bits — far inside the tolerance the Go tests use.
"""

from __future__ import annotations

import io

import numpy as np
from PIL import Image

IMAGE_SIZE = 224


def resize_to_fill(image: Image.Image, size: int = IMAGE_SIZE) -> Image.Image:
    """Scale so the short side covers size, then centre-crop the overflow."""
    width, height = image.size
    scale = max(size / width, size / height)
    scaled = (max(round(width * scale), size), max(round(height * scale), size))
    resized = image.resize(scaled, resample=Image.Resampling.BILINEAR)
    left, top = (scaled[0] - size) // 2, (scaled[1] - size) // 2
    return resized.crop((left, top, left + size, top + size))


def preprocess(data: bytes) -> np.ndarray:
    """Decoded image bytes -> (3, 224, 224) float32 pixels in [-1, 1]."""
    with Image.open(io.BytesIO(data)) as image:
        filled = resize_to_fill(image.convert("RGB"))
    pixels = np.asarray(filled, dtype=np.float32) / 127.5 - 1.0
    return pixels.transpose(2, 0, 1)
