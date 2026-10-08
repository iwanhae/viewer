"""Renders the probe's .npz results as a self-contained HTML gallery.

The retrieval-quality question can only be settled by looking at which photos
each model actually surfaces, so this turns every query's top-k into side-by-
side image cards. Thumbnails are embedded as base64 WebP so the report is one
portable file, viewable offline:

    probe.py report sig.npz eg2.npz            # numbers
    gallery.py sig.npz eg2.npz > report.html   # eyeball material
"""

from __future__ import annotations

import argparse
import base64
import io
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(Path(__file__).resolve().parent))
from probe import DEFAULT_PHOTOS, QUERY_PAIRS  # noqa: E402

from PIL import Image  # noqa: E402

THUMB = (240, 240)


def thumb_b64(path: Path) -> str:
    with Image.open(path) as im:
        im = im.convert("RGB")
        im.thumbnail(THUMB)
        buf = io.BytesIO()
        im.save(buf, "WEBP", quality=70)
    return base64.b64encode(buf.getvalue()).decode()


def main() -> None:
    import numpy as np

    parser = argparse.ArgumentParser()
    parser.add_argument("sig", type=Path)
    parser.add_argument("eg2", type=Path)
    parser.add_argument("--photos", type=Path, default=DEFAULT_PHOTOS)
    parser.add_argument("--top", type=int, default=5)
    args = parser.parse_args()

    sig = np.load(args.sig)
    eg2 = np.load(args.eg2)
    names = [str(n) for n in sig["names"]]
    assert list(names) == [str(n) for n in eg2["names"]], "photo sets differ"

    thumbs = {n: thumb_b64(args.photos / n) for n in names}

    def normalize(mat):
        mat = mat / np.clip(np.linalg.norm(mat, axis=-1, keepdims=True), 1e-12, None)
        return mat

    sig_img = normalize(sig["image_embs"])
    eg2_img = normalize(eg2["image_embs"])

    def top5(q, img_embs, k):
        sims = q @ img_embs.T
        order = np.argsort(-sims)[:k]
        return [(names[i], float(sims[i])) for i in order]

    cards = []
    for qi, (label, ko, en) in enumerate(QUERY_PAIRS):
        for lang, text, sig_q, eg2_q in (
            ("ko", ko, sig["q_ko_embs"], eg2["q_ko_embs"]),
            ("en", en, sig["q_en_embs"], eg2["q_en_embs"]),
        ):
            sq = normalize(sig_q[qi : qi + 1])
            eq = normalize(eg2_q[qi : qi + 1])
            rows = ""
            for model, hits in (
                ("SigLIP2", top5(sq[0], sig_img, args.top)),
                ("EmbeddingGemma 2", top5(eq[0], eg2_img, args.top)),
            ):
                cells = "".join(
                    f'<div class="hit"><img src="data:image/webp;base64,{thumbs[n]}">'
                    f'<span>{n[:10]}<br>{s:.3f}</span></div>'
                    for n, s in hits
                )
                rows += f'<div class="model"><b>{model}</b><div class="row">{cells}</div></div>'
            cards.append(
                f'<section><h2>{label} <small>— {text} ({lang})</small></h2>{rows}</section>'
            )

    html = f"""<!doctype html><meta charset="utf-8">
<title>SigLIP2 vs EmbeddingGemma 2</title>
<style>
body{{font-family:sans-serif;background:#111;color:#eee;max-width:1500px;margin:2rem auto;padding:0 1rem}}
h1 small{{color:#999}} section{{margin:2rem 0;padding:1rem;background:#1c1c1c;border-radius:8px}}
h2 small{{color:#999;font-weight:normal}} .model{{margin:.5rem 0}}
.row{{display:flex;gap:.5rem;flex-wrap:wrap}} .hit{{text-align:center;font-size:.7rem}}
.hit img{{width:120px;height:120px;object-fit:cover;border-radius:4px;display:block}}
</style>
<h1>SigLIP2 vs EmbeddingGemma 2 <small>{len(names)} photos, top {args.top}</small></h1>
{''.join(cards)}"""
    print(html)


if __name__ == "__main__":
    main()
