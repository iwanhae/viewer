"""Generate the PyTorch reference bundle used by internal/vision's tests.

Usage:
    SIGLIP2_MODEL=/path/to/model.safetensors SIGLIP2_CONFIG=/path/to/config.json \
        SIGLIP2_TOKENIZER=/path/to/tokenizer.json \
        VISION_OUT=/path/to/bundle python generate.py

The bundle is written to VISION_OUT and contains:

    model/config.json          copied from SIGLIP2_CONFIG
    model/model.safetensors    copied from SIGLIP2_MODEL
    model/tokenizer.json       copied from SIGLIP2_TOKENIZER
    vision_reference.json      expected embeddings keyed by case label
    text_reference.json        expected text embeddings, ids and masks
    refimg_<case>.png          the generated source images
    refpix_rtf_<case>.bin      [-1, 1] pixels through Go's resize-to-fill geometry
    refpixels_<case>.bin       the [-1, 1] pixels fed to the reference tower

Run it inside a venv with torch, safetensors, tokenizers, numpy and Pillow
installed, then point VISION_REFERENCE_DIR at the bundle to run
TestAgainstReference and TestTextAgainstReference. The text reference needs the
tokenizer; without one only the vision reference is regenerated and the stale
text_reference.json (if any) is left untouched.
"""

import json
import os
import shutil
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import numpy as np
import torch
from PIL import Image
from tokenizers import Tokenizer

from reference_model import (
    EOS_ID,
    LAYERS,
    MAXLEN,
    PAD_ID,
    build_image,
    load_model,
    load_text_model,
)

CASES = {"1000x300": (1000, 300), "640x480": (640, 480), "13x7": (13, 7)}
# The tower's input side; mirrors vision.InputImageSize.
IMAGE_SIZE = 224

# The texts the text reference is generated from. The ids are cross-checked
# against internal/textenc's committed fixtures (SIGLIP2_CASES), so these must
# stay byte-identical to the fixture texts.
TEXT_CASES = {
    "english": "sunset over mountains",
    "korean": "해변에서 노는 강아지",
    "mixed": "2023년 여름 제주도 여행 sunset",
    "emoji": "a cat 🐱 on the beach 🏖️",
    "empty": "",
    "long": (
        "the old lighthouse stood at the edge of the cliff, watching over the "
        "harbour the way it had for more than a century. every morning the "
        "keeper climbed the spiral stairs with a lantern in one hand and a "
        "thermos of coffee in the other, pausing on the landing to look at the "
        "sea. fishing boats left early, their lights still burning in the dark, "
        "and returned at dusk with the day's catch stacked in wooden boxes. "
        "children played on the stony beach below, collecting shells and smooth "
        "glass, while their dog chased the gulls that drifted over the water. "
        "in summer the island filled with visitors who walked the coastal path "
        "and photographed the wildflowers; in winter the wind swept everything "
        "clean and the lighthouse beam was the only thing moving against the "
        "stars. the keeper liked the winter best, when the island remembered "
        "its own rhythm and the sea spoke in its ordinary voice."
    ),
}


def resize_to_fill(image, size):
    """Scale so the short side covers size, then centre-crop the overflow."""
    width, height = image.size
    scale = max(size / width, size / height)
    scaled = (max(round(width * scale), size), max(round(height * scale), size))
    resized = image.resize(scaled, resample=Image.Resampling.BILINEAR)
    left, top = (scaled[0] - size) // 2, (scaled[1] - size) // 2
    return resized.crop((left, top, left + size, top + size))


def encode_text(tokenizer, text):
    """Reproduces internal/textenc's Encode: content tokens, exactly one
    trailing <eos>, right-padded with <pad> to MAXLEN.

    The checkpoint's tokenizer.json ships with fixed-64 padding configured, so
    the loaded tokenizer applies it inside encode() regardless of
    add_special_tokens. It is disabled here: the reference needs the bare
    content tokens so it can apply textenc's own guarantees — truncate the
    content to MAXLEN-1, append one <eos>, right-pad with <pad>.
    """
    content = list(tokenizer.encode(text, add_special_tokens=False).ids)
    content = content[: MAXLEN - 1]
    ids = content + [EOS_ID] + [PAD_ID] * (MAXLEN - len(content) - 1)
    mask = [1.0] * (len(content) + 1) + [0.0] * (MAXLEN - len(content) - 1)
    return ids, mask


def check_text_fixtures(cases):
    """Cross-checks the ground-truth ids against internal/textenc's committed
    fixtures and aborts on any divergence: the fixtures are verified
    id-for-id against HuggingFace's tokenizers library, so a mismatch here
    means this script and the Go tokenizer disagree about the encoding and the
    reference bundle would measure the wrong thing."""
    fixtures_path = os.environ.get(
        "SIGLIP2_CASES",
        os.path.join(
            os.path.dirname(os.path.abspath(__file__)),
            "..", "..", "internal", "textenc", "testdata", "cases.json",
        ),
    )
    if not os.path.exists(fixtures_path):
        raise SystemExit(f"text fixture file not found: {fixtures_path}")
    with open(fixtures_path) as handle:
        fixtures = {case["label"]: case for case in json.load(handle)}
    for label, case in cases.items():
        fixture = fixtures.get(label)
        if fixture is None:
            raise SystemExit(f"text case {label!r}: no fixture in {fixtures_path}")
        if fixture["text"] != case["text"]:
            raise SystemExit(
                f"text case {label!r}: text differs from the fixture in {fixtures_path}"
            )
        if fixture["ids"] != case["ids"]:
            raise SystemExit(
                f"text case {label!r}: ids {case['ids'][:8]}... differ from the "
                f"fixture ids {fixture['ids'][:8]}..."
            )
        if [float(value) for value in fixture["mask"]] != case["mask"]:
            raise SystemExit(f"text case {label!r}: mask differs from the fixture")
    print(f"text ids match {len(cases)} fixtures in {os.path.basename(fixtures_path)}")


def generate_text(out_dir, model_dir):
    """Generates text_reference.json next to vision_reference.json."""
    tokenizer_path = os.environ.get("SIGLIP2_TOKENIZER")
    if tokenizer_path:
        shutil.copyfile(tokenizer_path, os.path.join(model_dir, "tokenizer.json"))
    else:
        tokenizer_path = os.path.join(model_dir, "tokenizer.json")
    if not os.path.exists(tokenizer_path):
        print(
            "warning: no tokenizer.json available (set SIGLIP2_TOKENIZER); "
            "text_reference.json was not generated"
        )
        return

    tokenizer = Tokenizer.from_file(tokenizer_path)
    tokenizer.no_padding()
    tokenizer.no_truncation()
    text_model = load_text_model()
    out = {"text": {}}
    for label, text in TEXT_CASES.items():
        ids, mask = encode_text(tokenizer, text)
        with torch.no_grad():
            embedding = text_model(
                torch.tensor([ids], dtype=torch.long),
                torch.tensor([mask], dtype=torch.float32),
            )
        out["text"][label] = {
            "text": text,
            "ids": ids,
            "mask": mask,
            "embedding": embedding[0].tolist(),
        }
        print(f"{label}: ids[:6]={ids[:6]} embedding[:3]={out['text'][label]['embedding'][:3]}")

    # The ids are checked before anything is written: a bundle whose ids
    # disagree with the Go tokenizer is worse than no bundle at all.
    check_text_fixtures(out["text"])
    path = os.path.join(out_dir, "text_reference.json")
    with open(path, "w") as handle:
        json.dump(out, handle)
    print(f"wrote {path}")


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

    generate_text(out_dir, model_dir)


if __name__ == "__main__":
    main()
