"""Generate the PyTorch reference bundle used by internal/vision's tests.

Usage:
    SIGLIP2_MODEL=/path/to/model.safetensors SIGLIP2_CONFIG=/path/to/config.json \
        VISION_OUT=/path/to/bundle python generate.py

The bundle is written to VISION_OUT and contains:

    model/config.json          copied from SIGLIP2_CONFIG
    model/model.safetensors    copied from SIGLIP2_MODEL
    vision_reference.json      expected embeddings keyed by case label
    refimg_<case>.png          the generated source images
    refpix_rtf_<case>.bin      [-1, 1] pixels through Go's resize-to-fill geometry
    refpixels_<case>.bin       the [-1, 1] pixels fed to the reference tower

Run it inside a venv with torch, safetensors, numpy and Pillow installed, then
point VISION_REFERENCE_DIR at the bundle to run TestAgainstReference.
"""

import json
import os
import shutil
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import numpy as np
import torch
from PIL import Image

from reference_model import LAYERS, build_image, load_model

CASES = {"1000x300": (1000, 300), "640x480": (640, 480), "13x7": (13, 7)}
# The tower's input side; mirrors vision.InputImageSize.
IMAGE_SIZE = 224


def resize_to_fill(image, size):
    """Scale so the short side covers size, then centre-crop the overflow."""
    width, height = image.size
    scale = max(size / width, size / height)
    scaled = (max(round(width * scale), size), max(round(height * scale), size))
    resized = image.resize(scaled, resample=Image.Resampling.BILINEAR)
    left, top = (scaled[0] - size) // 2, (scaled[1] - size) // 2
    return resized.crop((left, top, left + size, top + size))


def main():
    model_path = os.environ.get("SIGLIP2_MODEL")
    out_dir = os.environ.get("VISION_OUT")
    if not model_path or not out_dir:
        raise SystemExit("SIGLIP2_MODEL and VISION_OUT must both be set")

    model_dir = os.path.join(out_dir, "model")
    os.makedirs(model_dir, exist_ok=True)
    shutil.copyfile(model_path, os.path.join(model_dir, "model.safetensors"))
    config_src = os.environ.get("SIGLIP2_CONFIG")
    if config_src:
        shutil.copyfile(config_src, os.path.join(model_dir, "config.json"))
    else:
        print("warning: SIGLIP2_CONFIG is unset; model/config.json was not copied")

    model = load_model()
    out = {"embedding": {}}

    for label, (width, height) in CASES.items():
        source = build_image(width, height)
        source.save(os.path.join(out_dir, f"refimg_{label}.png"))

        # Same geometry as the Go preprocessor; Pillow rounds independently, so
        # the Go test compares these element-wise instead of by hash.
        filled = resize_to_fill(source, IMAGE_SIZE)
        pixels = np.asarray(filled, dtype=np.float32) / 127.5 - 1.0
        tower_pixels = pixels.transpose(2, 0, 1).astype(np.float32)
        tower_pixels.tofile(os.path.join(out_dir, f"refpix_rtf_{label}.bin"))
        # The pixels the reference feeds its own tower; kept separate so the Go
        # test can compare the tower without involving the resampler.
        tower_pixels.tofile(os.path.join(out_dir, f"refpixels_{label}.bin"))

        with torch.no_grad():
            embedding = model(torch.from_numpy(tower_pixels[None, ...]))
        out["embedding"][label] = embedding[0].tolist()
        print(f"{label}: embedding[:3]={out['embedding'][label][:3]}")

    with open(os.path.join(out_dir, "vision_reference.json"), "w") as handle:
        json.dump(out, handle)
    print(f"wrote {os.path.join(out_dir, 'vision_reference.json')}")
    print(f"reference layers: {LAYERS}")


if __name__ == "__main__":
    main()
