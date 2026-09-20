# viewer

Photo viewer MVP with:
- Go API server
- React frontend embedded into one Go binary
- Playwright e2e test flow

## Architecture

Uploads are still sent straight to object storage with a presigned `PUT`, but the
zip is only a transport container. A single in-process worker downloads each
staged zip sequentially, unpacks it entry by entry, and turns every image into a
content-addressed blob:

1. `POST /api/albums` registers a `PENDING` album in SQLite and returns a
   presigned `PUT` URL for `uploads/<albumId>/source.zip`.
2. The browser uploads the zip directly to S3.
3. `POST /api/albums/<albumId>/finalize` queues the album for the pipeline
   worker.
4. The worker downloads the zip to local disk and walks its image entries in
   filename order. For each entry it:
   - computes the SHA-256 of the raw image bytes,
   - reads the entry name and image dimensions,
   - stores the original bytes in S3 at `blobs/<sha256>` (uploaded only once per
     distinct content hash, so identical images never occupy storage twice),
   - computes an embedding and writes it to SQLite,
   - records the zip entry name, hash, width, height and ratio in SQLite.
5. After a successful extraction the staged zip is deleted from S3.

All metadata lives in the local SQLite catalog at `/tmp/viewer-cache/viewer.db`;
S3 only holds the staged zip (briefly) and the deduplicated image blobs. The
`albums/<id>/index.json` objects of previous versions are no longer written or
read.

Image requests are resolved as `(albumId, index)` -> photo row -> blob hash ->
`blobs/<hash>`, with a local disk cache in `/tmp/viewer-cache/images`.

## Configuration

The viewer is always deployed as the Docker image, so the whole configuration is
five environment variables:

- `S3_ENDPOINT`, `S3_BUCKET`, `S3_ACCESS_KEY`, `S3_SECRET_KEY` — required.
- `PORT` — the port the container listens on (default `8080`).

Everything else is a constant in `internal/config`: the path-style S3 addressing
and signing region, the 1 GiB upload limit, the 15 minute presign TTL, the
`/tmp/viewer-cache` state directory, and the `/app/siglip2` checkpoint location.
Copy `.env.example` for a local run or `.env.test.example` for `make test`.

There is no separate inference service to run: `viewer` loads the checkpoint
from `/app/siglip2` once at startup and runs the vision tower in-process. The
tower needs no network access at startup, and a checkpoint that fails to load
only degrades: the container logs the failure, keeps serving, and returns
recommendations from whatever embeddings the catalog already holds. Mount a
directory at `/app/siglip2` (holding `config.json` and `model.safetensors`) to
supply a checkpoint to `runtime-slim`.

Embeddings are stored as `float32` blobs on each image blob row in SQLite. The
ingest pipeline embeds images inline; the background workers pick up any blob
left in the `pending` state (for example when the checkpoint could not be
loaded). Recommendation responses are cross-album only: photos from the
same album as the query are excluded from results. If no cross-album neighbors
exist for an embedded query photo, recommendations return an empty `items` list.

Docker:
- `runtime` (default `docker build .`) is self-contained: the Go viewer, frontend
  assets, and the prefetched SigLIP2 checkpoint at `/app/siglip2`.
- `runtime-slim` omits the checkpoint for deployments that mount one at
  `/app/siglip2`.
- CI publishes the viewer as `ghcr.io/<owner>/<repo>`.

The checkpoint is fetched at build time through the `model-prefetch` stage, so
pod startup does not require Hugging Face egress. To use a different model in
Docker, build with `--build-arg SIGLIP2_MODEL_ID=<repo-id>`.

## Commands
- `make build` compiles `bin/viewer` plus `bin/album-dedupe-cleaner`, rebuilding frontend assets only when their sources are newer than `internal/web/static` (`make build FORCE=1` forces a frontend rebuild).
- `make test` runs fast Go unit/integration tests only (`go test ./cmd/... ./internal/...`) using values from `.env.test`.
- `make test-full` runs the full regression pipeline: `make build`, `make test`, then Playwright e2e (screenshots saved to `samples/` by default).
- `make test-full` binds the app to `TEST_PORT` (default `18080`) and sets `E2E_BASE_URL` automatically.
- `make run` starts `bin/viewer` (loads `.env` if present, does not rebuild binaries).
- `make clean` removes build outputs, dependency caches, and the host-side `/tmp/viewer-cache` state directory.

Batch ingest:
- Any `batch/*.zip` object is copied to `uploads/<albumId>/source.zip` (with `albumId` derived from the zip content) and queued for extraction; the batch object is then removed. Re-uploading identical bytes is deduplicated.

Legacy batch duplicate cleanup binary (operates on pre-catalog `albums/<id>/source.zip` objects):
- `bin/album-dedupe-cleaner plan --out ./dedupe-plan.json` creates a deletion plan (no deletes).
- `bin/album-dedupe-cleaner apply --plan ./dedupe-plan.json` validates the plan snapshot and deletes duplicate album prefixes.

## Verifying the vision port

`internal/vision` reimplements the SigLIP2 vision tower. Its accuracy is checked
against an independent PyTorch implementation of the same checkpoint:

```
python -m venv /tmp/sigref-venv
/tmp/sigref-venv/bin/pip install torch safetensors numpy Pillow
SIGLIP2_MODEL=/path/to/model.safetensors \
SIGLIP2_CONFIG=/path/to/config.json \
VISION_OUT=/tmp/vision-bundle \
    /tmp/sigref-venv/bin/python scripts/vision-reference/generate.py
VISION_REFERENCE_DIR=/tmp/vision-bundle go test ./internal/vision/ -v
```

`generate.py` writes the source images, the pixels, and the expected embeddings
into the bundle; `TestAgainstReference` then checks the resampler against Pillow
(within one 8-bit level per channel) and the tower against PyTorch (max absolute
difference below `1e-4`, cosine above `0.9995`). The test is skipped unless
`VISION_REFERENCE_DIR` is set, because it needs the 1.5 GB checkpoint.

## Observability
- The server logs to stdout/stderr via Go's standard logger.
- Startup warmup loads the SQLite catalog into the recommendation index, scans the `batch/` prefix, and enqueues any album whose staged zip is still waiting.
- Embedding progress metrics (`/metrics`) are computed from the SQLite catalog.
- Request-scoped 500 errors now include request context including:
  - request method/path
  - request ID (Chi request ID middleware)
  - remote IP
  - raw query
  - detailed internal error message
- Panics are logged with stack traces before the 500 response is returned.
