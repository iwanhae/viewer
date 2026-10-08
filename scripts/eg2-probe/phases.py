"""Phase drivers for the GB10 overnight EG2 study (image-image similarity).

Everything here is image-to-image: the txt-img query machinery in probe.py is
deliberately unused. Each subcommand is idempotent (skips when its summary.json
exists, --force to rerun) and writes:

    .tmp/eg2probe/overnight/<phase>/summary.json   key metrics, printed also
    .tmp/eg2probe/overnight/<phase>/*.npz          vectors where applicable

Phases run on the DGX Spark (wan@192.168.0.110, ~/git/viewer). Subagents drive
them over ssh; the orchestrator only reads the summaries afterwards.
"""

from __future__ import annotations

import argparse
import io
import json
import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from urllib.request import urlopen

import numpy as np

REPO = Path(__file__).resolve().parents[2]
OUT = REPO / ".tmp" / "eg2probe" / "overnight"
PHOTO_DIR = REPO / ".tmp" / "eg2probe" / "photos"
EG2_DIR = REPO / ".tmp" / "eg2probe" / "eg2"
SIG_DIR = REPO / ".tmp" / "eg2probe" / "siglip2"
PHOTOS_META = OUT / "photos.json"

SITE = "https://tmp.iwanhae.kr"
PHOTOS_N = 1500
IMAGE_W = 1024  # scaled variant; originals stay on the server
KNN_K = 10


def log(msg: str) -> None:
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


def finish(phase: str, summary: dict, force: bool) -> dict:
    """Persists summary.json unless it already exists; returns it either way."""
    out = OUT / phase
    out.mkdir(parents=True, exist_ok=True)
    path = out / "summary.json"
    if path.exists() and not force:
        return json.loads(path.read_text())
    path.write_text(json.dumps(summary, indent=2, ensure_ascii=False))
    return summary


def photos() -> list[dict]:
    if not PHOTOS_META.exists():
        raise SystemExit(f"no {PHOTOS_META}; run the fetch phase first")
    return json.loads(PHOTOS_META.read_text())["photos"]


def present_photos(meta: list[dict]) -> list[tuple[str, Path]]:
    pairs = [(m["hash"], PHOTO_DIR / f"{m['hash']}.jpg") for m in meta]
    missing = [str(p) for _, p in pairs if not p.exists()]
    if missing:
        raise SystemExit(f"{len(missing)} photos missing (e.g. {missing[0]}); rerun fetch")
    return pairs


def l2norm(mat: np.ndarray) -> np.ndarray:
    return mat / np.clip(np.linalg.norm(mat, axis=-1, keepdims=True), 1e-12, None)


def knn(embs: np.ndarray, k: int = KNN_K) -> tuple[np.ndarray, np.ndarray]:
    """Top-k neighbour indices and cosine scores for every row, self excluded."""
    embs = l2norm(embs)
    n = len(embs)
    idx = np.zeros((n, k), dtype=np.int32)
    sims = np.zeros((n, k), dtype=np.float32)
    block = 512
    for s in range(0, n, block):
        rows = embs[s : s + block] @ embs.T
        for i in range(s, min(s + block, n)):
            rows[i - s, i] = -2.0
        part = np.argpartition(-rows, kth=k, axis=1)[:, :k]
        ps = np.take_along_axis(rows, part, axis=1)
        order = np.argsort(-ps, axis=1)
        idx[s : s + block] = np.take_along_axis(part, order, axis=1)
        sims[s : s + block] = np.take_along_axis(ps, order, axis=1)
    return idx, sims


def knn_agreement(a: np.ndarray, b: np.ndarray, k: int = KNN_K) -> float:
    ia, _ = knn(a, k)
    ib, _ = knn(b, k)
    return float(np.mean([len(set(ra[:k]) & set(rb[:k])) / k for ra, rb in zip(ia, ib)]))


def pair_corr(a: np.ndarray, b: np.ndarray, samples: int = 200_000, seed: int = 0) -> float:
    """Pearson correlation of pairwise cosines between two embedding spaces."""
    rng = np.random.default_rng(seed)
    n = len(a)
    i = rng.integers(0, n, samples)
    j = rng.integers(0, n, samples)
    keep = i != j
    i, j = i[keep], j[keep]
    a, b = l2norm(a), l2norm(b)
    sa = (a[i] * a[j]).sum(1)
    sb = (b[i] * b[j]).sum(1)
    return float(np.corrcoef(sa, sb)[0, 1])


# ---------------------------------------------------------------- fetch ----

def cmd_fetch(args) -> dict:
    """Builds the photo corpus from the viewer API. Idempotent on downloads.

    The feed lists one cover photo per album, so albumId would be a useless
    label if we only took feed items. Instead we walk the feed for album ids
    (cursor = response nextCursor, NOT the photo hash) and then pull every
    album's detail endpoint to get the photos inside it. That yields real
    multi-photo album labels for the clustering phase.
    """
    cursor, album_ids = "", []
    while len(album_ids) < args.albums:
        url = f"{SITE}/api/feed?mode=latest&limit=200" + (f"&after={cursor}" if cursor else "")
        with urlopen(url, timeout=30) as r:
            page = json.loads(r.read())
        album_ids.extend(item["albumId"] for item in page["items"])
        cursor = page.get("nextCursor", "")
        if not page.get("hasNext") or not cursor:
            break
        if len(album_ids) % 2000 < 200:
            log(f"album walk: {len(album_ids)} albums")
    album_ids = list(dict.fromkeys(album_ids))[: args.albums]
    log(f"album walk done: {len(album_ids)} albums")

    def album_detail(album_id: str):
        try:
            with urlopen(f"{SITE}/api/albums/{album_id}", timeout=30) as r:
                return json.loads(r.read())
        except Exception as exc:  # noqa: BLE001 - logged, album skipped
            log(f"album detail {album_id[:8]}: {exc}")
            return None

    meta, seen, failed_albums = [], set(), 0
    with ThreadPoolExecutor(8) as pool:
        for detail in pool.map(album_detail, album_ids):
            if detail is None:
                failed_albums += 1
                continue
            taken = 0
            for ph in detail.get("photos", []):
                h = ph.get("hash")
                if not h or h in seen:
                    continue
                seen.add(h)
                meta.append({"hash": h, "albumId": detail["albumId"], "i": ph.get("i", 0),
                             "file": f"{h}.jpg"})
                taken += 1
                if taken >= args.per_album or len(meta) >= args.n:
                    break
            if len(meta) >= args.n:
                break
    log(f"catalog: {len(meta)} photos across {len({m['albumId'] for m in meta})} albums")
    OUT.mkdir(parents=True, exist_ok=True)
    PHOTOS_META.write_text(json.dumps({"site": SITE, "fetched_at": time.time(),
                                       "albums": len(album_ids), "photos": meta}))

    todo = [m for m in meta if not (PHOTO_DIR / m["file"]).exists()]
    log(f"downloading {len(todo)}/{len(meta)} photos at w={IMAGE_W}")
    PHOTO_DIR.mkdir(parents=True, exist_ok=True)

    def grab(m):
        url = f"{SITE}/api/image/{m['hash']}?w={IMAGE_W}"
        try:
            with urlopen(url, timeout=60) as r:
                (PHOTO_DIR / m["file"]).write_bytes(r.read())
            return None
        except Exception as exc:  # noqa: BLE001 - logged and retried once
            return f"{m['hash'][:8]}: {exc}"

    failures = []
    with ThreadPoolExecutor(8) as pool:
        for res in pool.map(grab, todo):
            if res:
                failures.append(res)
    if failures:
        log(f"retrying {len(failures)} failed downloads once")
        retry = [m for m in todo if not (PHOTO_DIR / m["file"]).exists()]
        with ThreadPoolExecutor(4) as pool:
            for res in pool.map(grab, retry):
                if res:
                    failures.append(res)
    # Prune photos that could not be downloaded so every downstream phase sees
    # a consistent, fully-present corpus (npz names, album labels, pools).
    present = [m for m in meta if (PHOTO_DIR / m["file"]).exists()]
    dropped = len(meta) - len(present)
    if dropped:
        log(f"pruning {dropped} undownloadable photos from photos.json")
        PHOTOS_META.write_text(json.dumps({"site": SITE, "fetched_at": time.time(),
                                           "albums": len(album_ids), "photos": present}))
    summary = {
        "albums": len(album_ids), "failed_albums": failed_albums,
        "photos": len(present), "downloaded": len(todo) - dropped, "dropped": dropped,
        "bytes": sum((PHOTO_DIR / m["file"]).stat().st_size for m in present),
    }
    return finish("fetch", summary, args.force)


# ------------------------------------------------------------- eg2 embed ----

def load_eg2(dtype, device):
    import torch
    from transformers import AutoModel, AutoProcessor

    model = AutoModel.from_pretrained(str(EG2_DIR), dtype=dtype).to(device).eval()
    proc = AutoProcessor.from_pretrained(str(EG2_DIR))
    return model, proc


def eg2_embed(model, proc, paths, budget=280, batch=32, device="cuda"):
    """Returns ((N,768) fp32, ms/img) for EG2 image embeddings."""
    import torch
    import torch.nn.functional as F
    from PIL import Image

    embs, t0 = [], time.time()
    with torch.no_grad():
        for s in range(0, len(paths), batch):
            imgs = [Image.open(p).convert("RGB") for p in paths[s : s + batch]]
            inp = proc(images=[[i] for i in imgs], return_tensors="pt", max_soft_tokens=budget)
            inp = {k: v.to(device) for k, v in inp.items()}
            out = model(**inp)
            m = inp["attention_mask"].float().unsqueeze(-1)
            e = (out.last_hidden_state.float() * m).sum(1) / m.sum(1).clamp(min=1)
            embs.append(F.normalize(e, dim=-1).cpu().numpy().astype(np.float32))
    ms = (time.time() - t0) * 1000 / len(paths)
    return np.concatenate(embs), ms


def save_npz(phase: str, name: str, names: list[str], embs: np.ndarray, meta: dict) -> Path:
    out = OUT / phase
    out.mkdir(parents=True, exist_ok=True)
    path = out / name
    np.savez_compressed(path, names=np.array(names), embs=embs, **meta)
    return path


def cmd_calibrate(args) -> dict:
    import torch

    device = "cuda"
    model, proc = load_eg2(torch.bfloat16, device)
    pairs = present_photos(photos())
    results = {}
    for batch in args.batches:
        subset = [p for _, p in pairs[: batch * 3]]
        _, ms = eg2_embed(model, proc, subset, budget=args.budget, batch=batch, device=device)
        peak = torch.cuda.max_memory_allocated() / 2**30
        torch.cuda.reset_peak_memory_stats()
        results[f"batch{batch}"] = {"ms_per_img": round(ms, 1), "vram_gib": round(peak, 2)}
        log(f"batch {batch}: {ms:.1f} ms/img, peak {peak:.2f} GiB")
    summary = {
        "device": torch.cuda.get_device_name(0),
        "torch": torch.__version__,
        "budget": args.budget,
        "results": results,
    }
    return finish("calibrate", summary, args.force)


def cmd_embed(args) -> dict:
    import torch

    tag = args.out or f"b{args.budget}"
    phase = f"embed_{tag}"
    model, proc = load_eg2(torch.bfloat16, "cuda")
    pairs = present_photos(photos())[: args.subset or None]
    names = [h for h, _ in pairs]
    embs, ms = eg2_embed(model, proc, [p for _, p in pairs], budget=args.budget, batch=args.batch)
    save_npz(phase, f"eg2_{tag}.npz", names, embs,
             {"budget": args.budget, "ms_per_img": round(ms, 1)})
    summary = {"budget": args.budget, "out": tag, "photos": len(names),
               "ms_per_img": round(ms, 1), "img_per_s": round(1000 / ms, 2)}
    log(json.dumps(summary))
    return finish(phase, summary, args.force)


# ------------------------------------------------------------------ mrl ----

def cmd_mrl(args) -> dict:
    data = np.load(OUT / f"embed_b{args.base}" / f"eg2_b{args.base}.npz")
    full = data["embs"]
    idx, sims = knn(full)
    summary = {
        "base_budget": args.base,
        "photos": len(full),
        "full": {
            "neighbor_cos_mean": float(sims[:, :KNN_K].mean()),
            "neighbor_cos_p10": float(np.percentile(sims[:, 0], 10)),
        },
        "truncations": {},
    }
    for d in args.dims:
        trunc = l2norm(full[:, :d].copy())
        agree = knn_agreement(trunc, full)
        iidx, isims = knn(trunc)
        corr = pair_corr(trunc, full)
        summary["truncations"][str(d)] = {
            "knn10_agreement": round(agree, 4),
            "pair_corr": round(corr, 4),
            "neighbor_cos_mean": float(isims[:, :KNN_K].mean()),
            "storage_ratio": round(d / full.shape[1], 3),
        }
        log(f"{d}d: knn agreement {agree:.3f}, pair corr {corr:.4f}")
    return finish("mrl", summary, args.force)


# ------------------------------------------------------------- robustness --

def variants(img):
    """Byte-level and geometric variants mirroring the production pipeline."""
    from PIL import Image

    out = {}
    buf = io.BytesIO()
    img.save(buf, "WEBP", quality=85)  # matches the encoder worker's cwebp -q 85
    out["webp85"] = Image.open(buf).convert("RGB")
    buf = io.BytesIO()
    img.save(buf, "JPEG", quality=50)
    out["jpeg50"] = Image.open(buf).convert("RGB")
    small = img.copy()
    small.thumbnail((512, 512))
    out["resized512"] = small.convert("RGB")
    w, h = img.size
    out["crop90"] = img.crop((w // 20, h // 20, w - w // 20, h - h // 20))
    out["grayscale"] = img.convert("L").convert("RGB")
    return out


def cmd_robust(args) -> dict:
    import torch

    model, proc = load_eg2(torch.bfloat16, "cuda")
    rng = np.random.default_rng(0)
    meta = photos()
    chosen = rng.choice(len(meta), size=min(args.n, len(meta)), replace=False)
    chosen_meta = [meta[i] for i in chosen]
    base_names = [m["hash"] for m in meta]
    name_to_row = {h: i for i, h in enumerate(base_names)}

    corpus = np.load(OUT / f"embed_b{args.budget}" / f"eg2_b{args.budget}.npz")["embs"]
    idx, sims = knn(corpus)

    from PIL import Image

    per_variant = {v: {"cos": [], "self_rank1": 0, "top1_cos": []} for v in
                   ("webp85", "jpeg50", "resized512", "crop90", "grayscale")}
    for m in chosen_meta:
        orig_row = name_to_row[m["hash"]]
        with Image.open(PHOTO_DIR / m["file"]) as img:
            img = img.convert("RGB")
        vimgs = variants(img)
        vpaths = []
        for vname, vimg in vimgs.items():
            p = OUT / "robust" / f"{m['hash']}_{vname}.jpg"
            p.parent.mkdir(parents=True, exist_ok=True)
            vimg.save(p, "JPEG", quality=90)
            vpaths.append((vname, p))
        embs, _ = eg2_embed(model, proc, [p for _, p in vpaths], budget=args.budget, batch=10)
        for (vname, _), emb in zip(vpaths, embs):
            row = name_to_row[m["hash"]]
            cos = float(emb @ l2norm(corpus)[row])
            # self-retrieval: nearest corpus neighbour of the variant
            score = l2norm(corpus) @ l2norm(emb[None])[0]
            score[row] = -2  # can't retrieve the exact row being tested
            top1 = int(np.argmax(score))
            per_variant[vname]["cos"].append(cos)
            per_variant[vname]["top1_cos"].append(float(score[top1]))
            per_variant[vname]["self_rank1"] += int(top1 == orig_row)

    summary = {"budget": args.budget, "photos": len(chosen_meta), "variants": {}}
    for vname, acc in per_variant.items():
        cos = np.array(acc["cos"])
        summary["variants"][vname] = {
            "cos_vs_orig_mean": round(float(cos.mean()), 4),
            "cos_vs_orig_p5": round(float(np.percentile(cos, 5)), 4),
            "self_rank1_recall": round(acc["self_rank1"] / len(chosen_meta), 4),
            "top1_neighbor_cos_mean": round(float(np.mean(acc["top1_cos"])), 4),
        }
        log(f"{vname}: {summary['variants'][vname]}")
    # baseline: how similar are two *different* random photos?
    rng = np.random.default_rng(1)
    i, j = rng.integers(0, len(corpus), 5000), rng.integers(0, len(corpus), 5000)
    keep = i != j
    diff = (l2norm(corpus)[i[keep]] * l2norm(corpus)[j[keep]]).sum(1)
    summary["random_pair_cos"] = {"mean": round(float(diff.mean()), 4), "p99": round(float(np.percentile(diff, 99)), 4)}
    return finish("robust", summary, args.force)


# ------------------------------------------------------------- precision ---

def cmd_precision(args) -> dict:
    import torch

    bf = np.load(OUT / f"embed_b{args.budget}" / f"eg2_b{args.budget}.npz")
    names_all = [str(n) for n in bf["names"]]
    rng = np.random.default_rng(7)
    pick = rng.choice(len(names_all), size=min(args.n, len(names_all)), replace=False)
    name_to_row = {h: i for i, h in enumerate(names_all)}
    pairs = present_photos(photos())
    subset = [pairs[i] for i in pick]

    # determinism: same input twice through the same bf16 model
    model, proc = load_eg2(torch.bfloat16, "cuda")
    e1, _ = eg2_embed(model, proc, [p for _, p in subset[:64]], budget=args.budget)
    e2, _ = eg2_embed(model, proc, [p for _, p in subset[:64]], budget=args.budget)
    det = float(np.abs(e1 - e2).max())

    # bf16 (corpus) vs fp32 agreement
    del model
    model, proc = load_eg2(torch.float32, "cuda")
    e32, ms32 = eg2_embed(model, proc, [p for _, p in subset], budget=args.budget)
    e16 = bf["embs"][[name_to_row[str(n)] for n in np.array(names_all)[pick]]]
    cos = (l2norm(e16) * l2norm(e32)).sum(1)
    agree = knn_agreement(e16, e32)
    summary = {
        "budget": args.budget, "subset": len(subset),
        "bf16_determinism_max_abs_diff": det,
        "bf16_vs_fp32": {
            "cos_mean": round(float(cos.mean()), 5),
            "cos_min": round(float(cos.min()), 5),
            "knn10_agreement": round(agree, 4),
            "interchangeable_at_09995": bool(cos.min() >= 0.9995),
            "fp32_ms_per_img": round(ms32, 1),
        },
    }
    return finish("precision", summary, args.force)


# -------------------------------------------------------------- siglip2 ----

def cmd_siglip2(args) -> dict:
    import sys as _sys
    import torch

    _sys.path.insert(0, str(REPO / "scripts" / "vision-reference"))
    os.environ["SIGLIP2_MODEL"] = str(SIG_DIR / "model.safetensors")
    import reference_model as ref
    from PIL import Image

    vision = ref.load_model().eval().to("cuda")
    pairs = present_photos(photos())
    names = [h for h, _ in pairs]
    embs, t0 = [], time.time()
    with torch.no_grad():
        for p in [q for _, q in pairs]:
            pixels = ref.normalise(ref.raw_pixels(Image.open(p).convert("RGB")))
            embs.append(vision(torch.tensor(pixels).cuda())[0].cpu().numpy().astype(np.float32))
    ms = (time.time() - t0) * 1000 / len(names)
    embs = np.stack(embs)
    save_npz("siglip2", "siglip2.npz", names, embs, {"ms_per_img": round(ms, 1)})

    summary = {"photos": len(names), "ms_per_img": round(ms, 1), "img_per_s": round(1000 / ms, 2)}
    base_path = OUT / f"embed_b{args.eg2_budget}" / f"eg2_b{args.eg2_budget}.npz"
    if base_path.exists():
        eg2 = np.load(base_path)["embs"]
        agree = knn_agreement(embs, eg2)
        i_s, s_s = knn(embs)
        i_e, s_e = knn(eg2)
        summary["vs_eg2"] = {
            "knn10_agreement": round(agree, 4),
            "pair_corr": round(pair_corr(embs, eg2), 4),
            "eg2_top1_cos_mean": round(float(s_e[:, 0].mean()), 4),
            "siglip2_top1_cos_mean": round(float(s_s[:, 0].mean()), 4),
        }
    return finish("siglip2", summary, args.force)


# ------------------------------------------------------------- sustained ---

def cmd_sustained(args) -> dict:
    """Continuous embedding for sustained-throughput measurement.

    Unlike the ms/img numbers from short runs this answers the production
    question: what rate does GB10 hold over an hour, at what power, and what
    that implies for re-embedding tens of millions of photos. Images are drawn
    randomly (with replacement) from the pool so nothing is cached-friendly.
    """
    import subprocess
    import threading
    import torch

    model, proc = load_eg2(torch.bfloat16, "cuda")
    meta = photos()[: args.pool]
    pairs = present_photos(meta)
    paths = [p for _, p in pairs]
    rng = np.random.default_rng(3)
    out = OUT / "sustained"
    out.mkdir(parents=True, exist_ok=True)

    stop = threading.Event()
    samples = []

    def sampler():
        while not stop.is_set():
            try:
                q = subprocess.run(
                    ["nvidia-smi", "--query-gpu=power.draw,temperature.gpu,utilization.gpu,clocks.sm",
                     "--format=csv,noheader,nounits"],
                    capture_output=True, text=True, timeout=10)
                p, t, u, c = (float(x) for x in q.stdout.strip().split("\n")[0].split(","))
            except Exception:  # noqa: BLE001 - transient nvidia-smi hiccups are fine
                p = t = u = c = float("nan")
            samples.append((time.time(), p, t, u, c))
            stop.wait(15)

    thread = threading.Thread(target=sampler, daemon=True)
    thread.start()

    from PIL import Image
    import torch.nn.functional as F

    warmup_s, window_s = 120.0, 60.0
    started = time.time()
    stop_at = started + args.minutes * 60
    window_imgs, window_start = 0, started
    windows = []  # (window_start, imgs, rate)

    def decode(batch):
        return [Image.open(p).convert("RGB") for p in batch]

    with torch.no_grad(), ThreadPoolExecutor(args.loader_threads) as decoders:
        pending = decoders.submit(decode, [paths[i] for i in rng.integers(0, len(paths), args.batch)])
        while time.time() < stop_at:
            imgs = pending.result()
            pending = decoders.submit(decode, [paths[i] for i in rng.integers(0, len(paths), args.batch)])
            inp = proc(images=[[i] for i in imgs], return_tensors="pt", max_soft_tokens=args.budget)
            inp = {k: v.to("cuda") for k, v in inp.items()}
            outp = model(**inp)
            m = inp["attention_mask"].float().unsqueeze(-1)
            e = (outp.last_hidden_state.float() * m).sum(1) / m.sum(1).clamp(min=1)
            F.normalize(e, dim=-1)  # keep the math honest; vectors are not kept
            window_imgs += len(imgs)
            if time.time() - window_start >= window_s:
                windows.append((window_start, window_imgs, window_imgs / (time.time() - window_start)))
                log(f"sustained: window {len(windows)} rate {windows[-1][2]:.1f} img/s")
                window_start, window_imgs = time.time(), 0
    stop.set()
    thread.join(timeout=20)

    with open(out / "samples.csv", "w") as f:
        f.write("ts,power_w,temp_c,util_pct,clock_mhz\n")
        for row in samples:
            f.write(",".join(str(x) for x in row) + "\n")
    with open(out / "windows.csv", "w") as f:
        f.write("start,imgs,rate\n")
        for w in windows:
            f.write(",".join(str(x) for x in w) + "\n")

    steady = [w[2] for w in windows if w[0] - started >= warmup_s]
    rates = np.array(steady)
    power = [s[1] for s in samples if np.isfinite(s[1]) and s[0] - started >= warmup_s]
    rate = float(rates.mean()) if len(rates) else 0.0
    pw = float(np.mean(power)) if power else float("nan")
    summary = {
        "budget": args.budget, "batch": args.batch, "minutes": args.minutes,
        "windows": len(windows),
        "img_per_s": {"mean": round(rate, 2), "p50": round(float(np.median(rates)), 2) if len(rates) else None,
                      "p5": round(float(np.percentile(rates, 5)), 2) if len(rates) else None},
        "power_w_mean": round(pw, 1) if np.isfinite(pw) else None,
        "joules_per_1000_img": round(pw * 1000 / rate, 1) if rate else None,
        "hours_for_photos": {str(n): round(n / rate / 3600, 1) for n in (10_000_000, 30_000_000, 50_000_000)},
        "temp_c_max": max((s[2] for s in samples if np.isfinite(s[2])), default=None),
    }
    log(json.dumps(summary, ensure_ascii=False))
    return finish("sustained", summary, args.force)


# -------------------------------------------------------------- clustering --

def cmd_cluster(args) -> dict:
    """k-means clustering vs albumId weak ground truth across embedding configs."""
    from sklearn.cluster import KMeans
    from sklearn.metrics import adjusted_rand_score, normalized_mutual_info_score, silhouette_score

    meta = photos()
    name_to_album = {m["hash"]: m["albumId"] for m in meta}

    def load_config(path, dim=0):
        data = np.load(path)
        names = [str(n) for n in data["names"]]
        embs = data["embs"]
        if dim:
            embs = l2norm(embs[:, :dim].copy())
        return names, l2norm(embs)

    base = OUT
    configs = {
        "eg2_b280_768d": (base / "embed_b280" / "eg2_b280.npz", 0),
        "eg2_b280_256d": (base / "embed_b280" / "eg2_b280.npz", 256),
        "eg2_b280_128d": (base / "embed_b280" / "eg2_b280.npz", 128),
        "eg2_b70_768d": (base / "embed_b70" / "eg2_b70.npz", 0),
        "eg2_b1120_768d": (base / "embed_b1120" / "eg2_b1120.npz", 0),
        "siglip2_768d": (base / "siglip2" / "siglip2.npz", 0),
    }

    summary = {"k_grid": args.k, "configs": {}}
    medoids = None
    for cname, (path, dim) in configs.items():
        if not path.exists():
            log(f"skip {cname}: {path} missing (phase not run?)")
            continue
        names, embs = load_config(path, dim)
        albums = [name_to_album.get(n, "none") for n in names]
        entry = {}
        for k in args.k:
            km = KMeans(n_clusters=k, n_init=3, random_state=0, max_iter=100).fit(embs)
            nmi = normalized_mutual_info_score(albums, km.labels_)
            ari = adjusted_rand_score(albums, km.labels_)
            sil = silhouette_score(embs, km.labels_, sample_size=min(5000, len(embs)), random_state=0)
            entry[str(k)] = {"nmi": round(float(nmi), 4), "ari": round(float(ari), 4),
                             "silhouette": round(float(sil), 4)}
            log(f"{cname} k={k}: nmi={nmi:.3f} ari={ari:.3f} sil={sil:.3f}")
        summary["configs"][cname] = entry
        if cname == "eg2_b280_768d" and 200 in args.k:
            km = KMeans(n_clusters=200, n_init=3, random_state=0, max_iter=100).fit(embs)
            cent = l2norm(km.cluster_centers_)
            sims = embs @ cent.T  # (n, k) member-to-centroid similarity
            medoids = {}
            for c in range(200):
                top = np.argsort(-sims[:, c])[:5]
                medoids[str(c)] = [{"hash": names[i], "album": albums[i],
                                    "cos": round(float(sims[i, c]), 4)} for i in top]
    if medoids:
        (OUT / "cluster").mkdir(parents=True, exist_ok=True)
        (OUT / "cluster" / "medoids.json").write_text(json.dumps(medoids, ensure_ascii=False))
    return finish("cluster", summary, args.force)


# ------------------------------------------------------------------- cli ---

def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--force", action="store_true", help="rerun even if summary.json exists")
    sub = parser.add_subparsers(dest="phase", required=True)

    p = sub.add_parser("fetch")
    p.add_argument("--n", type=int, default=20000, help="target photo count")
    p.add_argument("--albums", type=int, default=5000, help="feed albums to walk")
    p.add_argument("--per-album", type=int, default=12, help="photos kept per album")
    p.set_defaults(func=cmd_fetch)

    p = sub.add_parser("calibrate")
    p.add_argument("--budget", type=int, default=280)
    p.add_argument("--batches", type=int, nargs="+", default=[8, 16, 32, 64])
    p.set_defaults(func=cmd_calibrate)

    p = sub.add_parser("embed")
    p.add_argument("--budget", type=int, default=280)
    p.add_argument("--batch", type=int, default=32)
    p.add_argument("--subset", type=int, default=0, help="embed only the first N photos (0=all)")
    p.add_argument("--out", default=None, help="output tag (default: b<budget>; use e.g. 'smoke' for tests)")
    p.set_defaults(func=cmd_embed)

    p = sub.add_parser("mrl")
    p.add_argument("--base", type=int, default=280)
    p.add_argument("--dims", type=int, nargs="+", default=[512, 256, 128])
    p.set_defaults(func=cmd_mrl)

    p = sub.add_parser("robust")
    p.add_argument("--budget", type=int, default=280)
    p.add_argument("--n", type=int, default=150)
    p.set_defaults(func=cmd_robust)

    p = sub.add_parser("precision")
    p.add_argument("--budget", type=int, default=280)
    p.add_argument("--n", type=int, default=300)
    p.set_defaults(func=cmd_precision)

    p = sub.add_parser("siglip2")
    p.add_argument("--eg2-budget", type=int, default=280)
    p.set_defaults(func=cmd_siglip2)

    p = sub.add_parser("sustained")
    p.add_argument("--minutes", type=int, default=90)
    p.add_argument("--pool", type=int, default=20000)
    p.add_argument("--budget", type=int, default=280)
    p.add_argument("--batch", type=int, default=32)
    p.add_argument("--loader-threads", type=int, default=8)
    p.set_defaults(func=cmd_sustained)

    p = sub.add_parser("cluster")
    p.add_argument("--k", type=int, nargs="+", default=[50, 200, 1000])
    p.add_argument("--force", action="store_true")
    p.set_defaults(func=cmd_cluster)

    args = parser.parse_args()
    result = args.func(args)
    print(json.dumps(result, indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
