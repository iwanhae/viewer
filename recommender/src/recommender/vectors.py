"""Vector <-> wire encoding: raw float32, little-endian, standard base64."""

from __future__ import annotations

import base64

import numpy as np


def encode(vector: np.ndarray) -> str:
    """Encode a float32 vector the way the server expects (little-endian b64).

    The vector is sent raw and unnormalized — the server persists exactly
    these bytes and normalizes only inside its in-memory similarity index.
    """
    return base64.b64encode(np.asarray(vector, dtype="<f4").tobytes()).decode("ascii")
