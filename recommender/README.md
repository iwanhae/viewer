# recommender

External embedding worker for the viewer photo server. It claims batches of
un-embedded blobs over the bearer-token worker API, fetches image bytes
straight from S3 via presigned GETs, runs the SigLIP2 vision tower
(`google/siglip2-base-patch16-224`, the same checkpoint the server's
built-in worker uses), and writes the raw 768-d vectors back in batches.

The model code and preprocessing are adapted from `scripts/vision-reference/`,
the PyTorch reference the Go server's `internal/vision` port is
regression-tested against (cosine >= 0.9995), so vectors produced here and by
the built-in worker are interchangeable.

## Setup

uv resolves torch from the index matching the machine it is synced on:

```sh
uv sync --extra cpu     # CPU-only box
uv sync --extra cu128   # CUDA 12.8 (driver >= ~570)
uv sync --extra cu126   # older CUDA 12.x drivers
```

Check `nvidia-smi` for the driver version if unsure. At runtime the device is
auto-detected; `--device cpu|cuda` overrides.

## Run

```sh
cp .env.example .env    # then fill in VIEWER_BASE_URL / VIEWER_WORKER_TOKEN
uv run recommender               # daemon: claim -> embed -> results, forever
uv run recommender --once        # single claim -> embed -> results cycle
uv run recommender --help        # all flags
```

The token must match the server's `EMBEDDING_WORKER_TOKEN`. If the repo root
`.env` already carries it, running from the repo root picks it up —
`EMBEDDING_WORKER_TOKEN` is accepted as a fallback for `VIEWER_WORKER_TOKEN`.

## How it behaves

- **Lease lifecycle**: a claim leases blobs for 10 minutes. Long batches are
  renewed automatically shortly before expiry; the server reports which
  leases actually renewed and anything not renewed is dropped locally.
- **Transient problems** (download 5xx/timeout/expired presign, GPU OOM at
  micro-batch 1) are skipped: the blob is simply omitted from the results,
  its lease expires within 10 minutes, and the next claim picks it up again.
- **Permanent problems** (undecodable/truncated image, non-finite output) are
  reported as terminal `failed` results so they are never re-claimed.
- **Ctrl-C / SIGTERM** finishes the in-flight micro-batch, posts its results,
  and exits; anything not embedded expires from its lease naturally.
- **401** exits immediately — the token is wrong.
- **`embeddingDim != 768`** at claim time exits immediately — wrong model for
  this server.
