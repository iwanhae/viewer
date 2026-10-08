"""Builds the report gallery and data tables from overnight phase outputs.

Runs on the Spark after the phases have finished:

    worker/.venv/bin/python scripts/eg2-probe/report.py

Produces, inside .tmp/eg2probe/overnight/report/:
    gallery.html  kNN neighbour grid + cluster medoid grid (thumbs are
                  referenced by relative path ../../photos/<hash>.jpg, so
                  serve the eg2probe dir or open over sshfs/maek)
    data.md       every summary table pre-rendered for the final report

The orchestrator writes the final report.md by hand from data.md.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))
from phases import OUT, knn, knn_agreement, l2norm  # noqa: E402

REPORT = OUT / "report"
PHOTO_REL = "../../photos/{}.jpg"
QDRANT_VECTORS_GB_FOR_30M = {768: 92.2, 512: 61.5, 256: 30.7, 128: 15.4}


def load_summary(phase: str) -> dict | None:
    path = OUT / phase / "summary.json"
    return json.loads(path.read_text()) if path.exists() else None


def load_npz(phase: str, name: str):
    path = OUT / phase / name
    if not path.exists():
        return None
    data = np.load(path)
    return [str(n) for n in data["names"]], data["embs"]


def md_table(header: list[str], rows: list[list]) -> str:
    out = ["| " + " | ".join(header) + " |", "|" + "---|" * len(header)]
    for row in rows:
        out.append("| " + " | ".join(str(x) for x in row) + " |")
    return "\n".join(out)


def fmt_hours(h: float) -> str:
    return f"{h / 24:.1f}일" if h >= 48 else f"{h:.1f}시간"


# ------------------------------------------------------------------ html ----

def img_tag(hash_: str, cos: float | None = None) -> str:
    label = f'<div class="cap">{hash_[:10]}…' + (f" · {cos:.3f}</div>" if cos is not None else "</div>")
    return f'<figure><img loading="lazy" src="{PHOTO_REL.format(hash_)}">{label}</figure>'


def build_gallery(anchors: list[dict], medoids: dict | None) -> str:
    css = """
    body{font-family:sans-serif;background:#111;color:#eee;margin:2rem}
    h2{margin-top:2.5rem;border-bottom:1px solid #444;padding-bottom:.3rem}
    .grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(160px,1fr));gap:.7rem}
    figure{margin:0}img{width:100%;height:130px;object-fit:cover;border-radius:4px;display:block}
    .cap{font-size:.7rem;color:#aaa;margin-top:.2rem;word-break:break-all}
    .anchor{margin-bottom:1.6rem}.anchor h3{margin:.4rem 0;font-size:.85rem;color:#7cc}
    """
    parts = [f"<html><head><meta charset='utf-8'><style>{css}</style></head><body>",
             "<h1>EG2 overnight — kNN &amp; cluster gallery</h1>",
             "<h2>kNN 이웃 (EG2, budget 280)</h2>"]
    for a in anchors:
        parts.append(f"<div class='anchor'><h3>앵커 {a['hash'][:12]}… (album {a['album']})</h3>"
                     + "<div class='grid'>" + img_tag(a["hash"])
                     + "".join(img_tag(h, c) for h, c in a["neighbors"]) + "</div></div>")
    if medoids:
        parts.append("<h2>클러스터 medoid (EG2 280 / 768d, k=200, 앞 40개)</h2>")
        for c, members in list(medoids.items())[:40]:
            parts.append(f"<div class='anchor'><h3>cluster {c}</h3><div class='grid'>"
                         + "".join(img_tag(m["hash"], m["cos"]) for m in members) + "</div></div>")
    parts.append("</body></html>")
    return "\n".join(parts)


# ------------------------------------------------------------------ data ----

def build_data_md() -> str:
    sections = []

    if cal := load_summary("calibrate"):
        sections.append("## 캘리브레이션 (EG2 bf16 @280)\n"
                        + md_table(["batch", "ms/img", "peak VRAM GiB"],
                                   [[k, v["ms_per_img"], v["vram_gib"]]
                                    for k, v in cal["results"].items()])
                        + f"\n\nGPU: {cal['device']}, torch {cal['torch']}")

    budget_rows, base = [], load_npz("embed_b280", "eg2_b280.npz")
    for b in (280, 70, 140, 560, 1120):
        s = load_summary(f"embed_b{b}")
        if not s:
            continue
        agree = ""
        if base and b != 280 and (cur := load_npz(f"embed_b{b}", f"eg2_b{b}.npz")):
            agree = f"{knn_agreement(cur[1], base[1]):.3f}"
        budget_rows.append([b, s["img_per_s"], s["ms_per_img"], agree])
    if budget_rows:
        sections.append("## 토큰 예산 스윕\n"
                        + md_table(["budget", "img/s", "ms/img", "kNN@10 일치율 vs 280"], budget_rows))

    if mrl := load_summary("mrl"):
        rows = [[d, t["knn10_agreement"], t["pair_corr"], t["neighbor_cos_mean"],
                 t["storage_ratio"], round(QDRANT_VECTORS_GB_FOR_30M[int(d)], 1) + " GB"]
                for d, t in mrl["truncations"].items()]
        sections.append("## MRL 절단\n"
                        + md_table(["dim", "kNN@10 일치", "pair corr", "이웃 cos 평균", "저장 비율", "30M 벡터 fp32"],
                                   rows)
                        + f"\n\n기준(768d): 이웃 cos 평균 {mrl['full']['neighbor_cos_mean']:.3f}, "
                          f"30M 벡터 ≈ {QDRANT_VECTORS_GB_FOR_30M[768]} GB (HNSW 그래프 별도)")

    if sus := load_summary("sustained"):
        r = sus["img_per_s"]
        h = sus["hours_for_photos"]
        sections.append("## 지속 성능 (90분)\n"
                        + md_table(["지표", "값"],
                                   [["img/s 평균(p50)", f"{r['mean']} ({r['p50']})"],
                                    ["img/s p5(최악 윈도우)", r["p5"]],
                                    ["평균 전력 W", sus["power_w_mean"]],
                                    ["J/1000장", sus["joules_per_1000_img"]],
                                    ["최고 온도 C", sus["temp_c_max"]],
                                    ["10M 재임베딩", fmt_hours(h["10000000"])],
                                    ["30M 재임베딩", fmt_hours(h["30000000"])],
                                    ["50M 재임베딩", fmt_hours(h["50000000"])]]))

    if cl := load_summary("cluster"):
        rows = [[c, k, v["nmi"], v["ari"], v["silhouette"]]
                for c, ks in cl["configs"].items() for k, v in ks.items()]
        sections.append("## 클러스터링 (albumId 약한 정답)\n"
                        + md_table(["구성", "k", "NMI", "ARI", "silhouette"], rows))

    if sig := load_summary("siglip2"):
        rows = [["SigLIP2", sig["img_per_s"], sig["ms_per_img"], "-"]]
        if "vs_eg2" in sig:
            v = sig["vs_eg2"]
            rows.append(["vs EG2 kNN@10 일치", "", "", f"{v['knn10_agreement']} (pair corr {v['pair_corr']})"])
            rows.append(["top1 cos 평균", "", "", f"sig {v['siglip2_top1_cos_mean']} / eg2 {v['eg2_top1_cos_mean']}"])
        sections.append("## SigLIP2 비교\n" + md_table(["항목", "img/s", "ms/img", "값"], rows))

    fetch = load_summary("fetch")
    if fetch:
        sections.insert(0, f"## 데이터\n사진 {fetch['photos']}장, {fetch['bytes'] / 2**20:.0f} MiB, "
                           f"실패 {fetch['failed']}")

    return "# data.md (자동 생성)\n\n" + "\n\n".join(sections)


def main() -> None:
    REPORT.mkdir(parents=True, exist_ok=True)
    anchors = []
    base = load_npz("embed_b280", "eg2_b280.npz")
    if base:
        names, embs = base
        idx, sims = knn(embs)
        import json as _json

        meta = {m["hash"]: m["albumId"] for m in
                _json.loads((OUT / "photos.json").read_text())["photos"]}
        rng = np.random.default_rng(11)
        for i in rng.choice(len(names), size=min(40, len(names)), replace=False):
            i = int(i)
            anchors.append({"hash": names[i], "album": meta.get(names[i], "?"),
                            "neighbors": [(names[j], float(sims[i, r])) for r, j in enumerate(idx[i])]})
    medoids_path = OUT / "cluster" / "medoids.json"
    medoids = _json.loads(medoids_path.read_text()) if medoids_path.exists() else None
    (REPORT / "gallery.html").write_text(build_gallery(anchors, medoids))
    (REPORT / "data.md").write_text(build_data_md())
    print(f"wrote {REPORT}/gallery.html ({len(anchors)} anchors) and data.md")


if __name__ == "__main__":
    main()
