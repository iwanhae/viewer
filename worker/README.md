# worker

One uv project provides two commands: `uv run recommender` for image
embeddings and `uv run encoder` for WebP Q85 compression. Both read
`VIEWER_BASE_URL` and `WORKER_TOKEN` from the environment or `worker/.env`.

Setting `WORKER_TOKEN` on the server enables WebP encoding. Use the same token
here. There is no separate WebP opt-out. Install the official `cwebp` CLI on
the encoder host. From `worker/`:

```sh
uv sync
uv run encoder                  # continuous claim -> encode -> complete loop
uv run encoder --workers 8      # number of concurrent cwebp processes
```

The server claims the largest pending source blobs first. The encoder runs
`cwebp -q 85 -alpha_q 100 -metadata all` and uploads only a smaller output to
a temporary key. The server validates it before replacing the original blob.
The output-size check includes copied EXIF, ICC, and XMP metadata. Already
WebP images and outputs that are not smaller remain untouched.

## Recommender

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
cp .env.example .env    # then fill in VIEWER_BASE_URL / WORKER_TOKEN
uv run recommender               # daemon: claim -> embed -> results, forever
uv run recommender --once        # single claim -> embed -> results cycle
uv run recommender --help        # all flags
```

The token must match the server's `WORKER_TOKEN`. The repo root `.env` is also
loaded when running from there.

## Throughput

The GPU is rarely the bottleneck — a round trip spends most of its time on
claim/results HTTP and pulling bytes from S3. To saturate it:

```sh
uv run recommender --claim-limit 512 --micro-batch 64 --download-workers 24
```

The `batch done` log line reports img/s and MiB/s. If raising
`--download-workers` stops moving MiB/s, the uplink to S3 is the ceiling;
the remaining levers are running the worker closer to the storage endpoint
or running several boxes that pull work from the same API.

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
