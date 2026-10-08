"""SigLIP2 vs EmbeddingGemma 2 offline search probe.

Compares the server's current checkpoint (google/siglip2-base-patch16-224)
against google/embeddinggemma-2 on the same photos and the same bilingual
queries, entirely offline, so a replacement decision rests on real retrieval
behaviour rather than leaderboard numbers.

The three stages run as separate processes so the two models never share RAM
(the SigLIP2 fp32 checkpoint alone is 1.4 GiB and the EG2 bf16 one is 1.5 GiB):

    probe.py siglip2 --photos DIR --out sig.npz [--limit 48]
    probe.py eg2     --photos DIR --out eg2.npz [--limit 48]
    probe.py report  sig.npz eg2.npz [--top 5]

Each stage writes the photo names, image embeddings, and query embeddings
(Korean and English variants) to its .npz. The report stage normalizes and
ranks, printing each query's top-k from both models plus the overlap.

Paths default to the layout this repo expects: the SigLIP2 checkpoint under
.tmp/eg2probe/siglip2 (mirrored from s3.iwanhae.kr), the EG2 snapshot under
.tmp/eg2probe/eg2, and the photo corpus under .tmp (the webp preset sources).
"""

from __future__ import annotations

import argparse
import os
import sys
import time
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
DEFAULT_PHOTOS = REPO / ".tmp" / "webp-preset-q85-20260924T031544Z" / "source"
DEFAULT_SIGLIP2 = REPO / ".tmp" / "eg2probe" / "siglip2"
DEFAULT_EG2 = REPO / ".tmp" / "eg2probe" / "eg2"

# Bilingual query pairs. Fifty intents spanning scenes, objects, people,
# food, activities, and weather, so overnight runs measure retrieval across a
# realistic intent spread instead of a handful of categories. Korean matters
# most: the SigLIP2 text tower is English-centric while EG2 trains on 100+
# languages.
QUERY_PAIRS = [
    ("snowy mountain", "눈 내린 산 봉우리", "snowy mountain peak"),
    ("sunset over the sea", "바다 노을", "sunset over the sea"),
    ("cat", "고양이", "a cat"),
    ("dog", "강아지", "a dog"),
    ("food", "맛있는 음식", "delicious food"),
    ("coffee", "커피 한 잔", "a cup of coffee"),
    ("flowers", "활짝 핀 꽃", "flowers in full bloom"),
    ("city night view", "도시 야경", "city skyline at night"),
    ("forest trail", "숲 속 산책길", "forest walking trail"),
    ("sky and clouds", "하늘과 구름", "sky and clouds"),
    ("crowded street", "붐비는 거리", "crowded street with people"),
    ("bicycle", "자전거", "a bicycle"),
    ("beach", "해변", "a sandy beach"),
    ("building architecture", "건물과 건축물", "building architecture"),
    ("waterfall or river", "계곡과 폭포", "waterfall in a valley"),
    ("autumn leaves", "가을 단풍", "autumn leaves"),
    ("hiking", "등산하는 사람", "people hiking"),
    ("camping tent", "캠핑 텐트", "a camping tent"),
    ("car", "자동차", "a car on the road"),
    ("bridge", "다리", "a bridge"),
    ("train station", "기차역 승강장", "a train station platform"),
    ("children", "아이들", "children playing"),
    ("family", "가족 모임", "a family gathering"),
    ("wedding", "결혼식", "a wedding ceremony"),
    ("night sky stars", "밤하늘 별", "starry night sky"),
    ("moon", "달", "the moon"),
    ("rainy day", "비 오는 날", "a rainy day"),
    ("fog", "안개 낀 풍경", "a foggy landscape"),
    ("snow", "눈 덮인 풍경", "a snow covered scene"),
    ("dessert", "디저트", "a dessert"),
    ("beer or drinks", "맥주와 안주", "beer and drinks"),
    ("market", "시장", "a street market"),
    ("shopping", "쇼핑", "a shopping street"),
    ("restaurant interior", "식당 내부", "a restaurant interior"),
    ("kitchen", "주방", "a kitchen"),
    ("living room", "거실", "a living room"),
    ("book and reading", "책과 독서", "reading a book"),
    ("lamp lighting", "조명", "a lamp with warm lighting"),
    ("painting or mural", "그림과 벽화", "a painting or mural"),
    ("sculpture", "조형물", "a sculpture"),
    ("bird", "새", "a bird"),
    ("insect butterfly", "나비", "a butterfly"),
    ("boat ship", "배", "a boat on the water"),
    ("swimming water", "물놀이", "playing in the water"),
    ("winter cold", "한겨울 풍경", "a deep winter scene"),
    ("spring cherry blossom", "봄 벚꽃", "cherry blossoms in spring"),
    ("rice meal", "밥상", "a korean meal with rice"),
    ("noodles", "면요리", "a noodle dish"),
    ("street food", "길거리 음식", "street food"),
    ("mirror selfie", "거울 셀카", "a mirror selfie"),
]

# EG2 was trained with task instruction prefixes on text. For image search
# the query side uses the SearchQuery prefix; images (the "documents") get
# no prefix at all.
EG2_QUERY_PREFIX = "task: search result | query: {q}"


def pick_photos(photos_dir: Path, limit: int) -> list[Path]:
    """Deterministic spread across the corpus: stride sampling over sorted names."""
    exts = {".jpg", ".jpeg", ".png", ".webp"}
    paths = sorted(p for p in photos_dir.iterdir() if p.suffix.lower() in exts)
    if not paths:
        raise SystemExit(f"no photos found in {photos_dir}")
    if limit < len(paths):
        stride = len(paths) / limit
        paths = [paths[int(i * stride)] for i in range(limit)]
    return paths


def save(out_path: Path, names: list[str], image_embs, q_ko_embs, q_en_embs) -> None:
    import numpy as np

    np.savez_compressed(
        out_path,
        names=np.array(names),
        image_embs=image_embs,
        queries_ko=np.array([q[1] for q in QUERY_PAIRS]),
        queries_en=np.array([q[2] for q in QUERY_PAIRS]),
        q_ko_embs=q_ko_embs,
        q_en_embs=q_en_embs,
    )
    print(f"wrote {out_path}: {len(names)} photos, {len(QUERY_PAIRS)} queries")


def load_pil(path: Path):
    from PIL import Image

    with Image.open(path) as im:
        return im.convert("RGB")


def stage_siglip2(args) -> None:
    sys.path.insert(0, str(REPO / "scripts" / "vision-reference"))
    import torch  # noqa: E402

    torch.set_num_threads(min(4, os.cpu_count() or 1))
    # reference_model reads SIGLIP2_MODEL at import time, so the env var must
    # be set before the import, not before the load.
    os.environ["SIGLIP2_MODEL"] = str(args.model_dir / "model.safetensors")
    import reference_model as ref  # noqa: E402  (path and env set above)
    from tokenizers import Tokenizer  # noqa: E402

    import numpy as np  # noqa: E402

    paths = pick_photos(args.photos, args.limit)

    # Vision pass. The reference towers are batch-1 by design; loop instead of
    # generalizing them, this probe is about retrieval quality not throughput.
    vision = ref.load_model().eval()
    image_embs = []
    started = time.time()
    with torch.no_grad():
        for i, path in enumerate(paths):
            pixels = ref.normalise(ref.raw_pixels(load_pil(path)))
            emb = vision(torch.tensor(pixels))
            image_embs.append(emb[0].numpy().astype(np.float32))
            if (i + 1) % 16 == 0:
                print(f"  siglip2 vision {i + 1}/{len(paths)}")
    del vision
    print(f"siglip2 vision done in {time.time() - started:.1f}s")

    # Text pass. Both language variants through the same tower so the report
    # can show how much the English-centric encoder loses on Korean.
    tokenizer = Tokenizer.from_file(str(args.model_dir / "tokenizer.json"))
    text_model = ref.load_text_model().eval()

    def encode_text(text):
        """Reproduces scripts/vision-reference/generate.py's encode_text and
        internal/textenc's Encode: content tokens truncated to MAXLEN-1,
        exactly one trailing <eos>, right-padded with <pad> to MAXLEN."""
        content = list(tokenizer.encode(text, add_special_tokens=False).ids)
        content = content[: ref.MAXLEN - 1]
        ids = content + [ref.EOS_ID] + [ref.PAD_ID] * (ref.MAXLEN - len(content) - 1)
        mask = [1.0] * (len(content) + 1) + [0.0] * (ref.MAXLEN - len(content) - 1)
        return ids, mask

    q_ko_embs, q_en_embs = [], []
    with torch.no_grad():
        for label, ko, en in QUERY_PAIRS:
            for out_list, text in ((q_ko_embs, ko), (q_en_embs, en)):
                ids, mask = encode_text(text)
                emb = text_model(
                    torch.tensor([ids], dtype=torch.long),
                    torch.tensor([mask], dtype=torch.float32),
                )
                out_list.append(emb[0].numpy().astype(np.float32))
    save(args.out, [p.name for p in paths], np.stack(image_embs), np.stack(q_ko_embs), np.stack(q_en_embs))


def stage_eg2(args) -> None:
    import numpy as np  # noqa: E402
    import torch  # noqa: E402
    import torch.nn.functional as F  # noqa: E402
    from transformers import AutoModel, AutoProcessor  # noqa: E402

    torch.set_num_threads(min(4, os.cpu_count() or 1))

    # The model card mandates bfloat16 or float32 (float16 overflows), and on
    # a CPU without bf16 vector units bf16 emulation is slower than fp32, so
    # the default follows the device: bf16 on CUDA, fp32 elsewhere.
    device = args.device or ("cuda" if torch.cuda.is_available() else "cpu")
    dtype = {"bf16": torch.bfloat16, "fp32": torch.float32}[args.dtype] if args.dtype else (
        torch.bfloat16 if device == "cuda" else torch.float32
    )
    print(f"eg2: device={device} dtype={dtype}")
    model = AutoModel.from_pretrained(str(args.model_dir), dtype=dtype).to(device)
    model.eval()
    processor = AutoProcessor.from_pretrained(str(args.model_dir))

    # Images are embedded at `max_soft_tokens` soft tokens each (70..1120,
    # default 280). Text passes stay unprefixed-budget: the knob only applies
    # to image batches.
    image_kwargs = {"max_soft_tokens": args.max_soft_tokens} if args.max_soft_tokens else {}

    def pool(last_hidden_state, attention_mask):
        mask = attention_mask.float().unsqueeze(-1)
        summed = (last_hidden_state.float() * mask).sum(dim=1)
        counts = mask.sum(dim=1).clamp(min=1)
        return F.normalize(summed / counts, dim=-1)

    paths = pick_photos(args.photos, args.limit)

    image_embs = []
    started = time.time()
    batch_size = 4
    with torch.no_grad():
        for start in range(0, len(paths), batch_size):
            batch = [load_pil(p) for p in paths[start : start + batch_size]]
            # The processor treats a flat list as ONE sample holding N images
            # (one embedding pooling all of them together); each photo must be
            # its own sample, so nest one image per sample.
            inputs = processor(images=[[img] for img in batch], return_tensors="pt", **image_kwargs)
            inputs = {k: v.to(device) for k, v in inputs.items()}
            out = model(**inputs)
            image_embs.append(pool(out.last_hidden_state, inputs["attention_mask"]).cpu().numpy())
            done = min(start + batch_size, len(paths))
            rate = done / (time.time() - started)
            print(f"  eg2 vision {done}/{len(paths)} ({rate:.2f} img/s)")
    print(f"eg2 vision done in {time.time() - started:.1f}s")

    q_ko = [EG2_QUERY_PREFIX.format(q=q[1]) for q in QUERY_PAIRS]
    q_en = [EG2_QUERY_PREFIX.format(q=q[2]) for q in QUERY_PAIRS]
    q_ko_embs, q_en_embs = [], []
    with torch.no_grad():
        for texts, sink in ((q_ko, q_ko_embs), (q_en, q_en_embs)):
            inputs = processor(text=texts, return_tensors="pt", padding=True)
            out = model(**inputs)
            sink.append(pool(out.last_hidden_state, inputs["attention_mask"]).numpy())
    save(
        args.out,
        [p.name for p in paths],
        np.concatenate(image_embs),
        q_ko_embs[0],
        q_en_embs[0],
    )


def stage_report(args) -> None:
    import numpy as np  # noqa: E402

    def load(path):
        data = np.load(path, allow_pickle=False)
        emb = {k: data[k] for k in ("image_embs", "q_ko_embs", "q_en_embs")}
        for mat in (*emb.values(),):
            norms = np.linalg.norm(mat, axis=-1, keepdims=True)
            mat /= np.clip(norms, 1e-12, None)
        return data["names"], data["queries_ko"], data["queries_en"], emb

    names_s, q_ko, q_en, sig = load(args.sig)
    names_e, _, _, eg2 = load(args.eg2)
    if list(names_s) != list(names_e):
        raise SystemExit("the two stages sampled different photos; rerun with the same --limit/--photos")

    top = args.top
    overlaps = []
    for qi, label in enumerate(QUERY_PAIRS):
        for lang, queries, s_embs, e_embs in (
            ("ko", q_ko, sig["q_ko_embs"], eg2["q_ko_embs"]),
            ("en", q_en, sig["q_en_embs"], eg2["q_en_embs"]),
        ):
            s_scores = s_embs[qi] @ sig["image_embs"].T
            e_scores = e_embs[qi] @ eg2["image_embs"].T
            s_top = np.argsort(-s_scores)[:top]
            e_top = np.argsort(-e_scores)[:top]
            overlap = len(set(s_top) & set(e_top))
            overlaps.append(overlap / top)
            print(f"\n[{label} / {lang}] overlap@{top} = {overlap}/{top}")
            print(f"  siglip2: " + ", ".join(
                f"{names_s[i][:10]} ({s_scores[i]:.3f})" for i in s_top))
            print(f"  eg2:     " + ", ".join(
                f"{names_e[i][:10]} ({e_scores[i]:.3f})" for i in e_top))

    print(f"\nmean overlap@{top}: {sum(overlaps) / len(overlaps):.2f} "
          f"(0 = completely different rankings, 1 = identical)")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="stage", required=True)

    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--photos", type=Path, default=DEFAULT_PHOTOS)
    common.add_argument("--limit", type=int, default=48)
    common.add_argument("--out", type=Path, required=True)

    p_sig = sub.add_parser("siglip2", parents=[common], help="embed with SigLIP2")
    p_sig.add_argument("--model-dir", type=Path, default=DEFAULT_SIGLIP2)
    p_sig.set_defaults(func=stage_siglip2)

    p_eg2 = sub.add_parser("eg2", parents=[common], help="embed with EmbeddingGemma 2")
    p_eg2.add_argument("--model-dir", type=Path, default=DEFAULT_EG2)
    p_eg2.set_defaults(func=stage_eg2)

    p_rep = sub.add_parser("report", help="compare the two .npz files")
    p_rep.add_argument("sig", type=Path)
    p_rep.add_argument("eg2", type=Path)
    p_rep.add_argument("--top", type=int, default=5)
    p_rep.set_defaults(func=stage_report)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
