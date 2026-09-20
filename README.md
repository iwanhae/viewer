# viewer

Photo viewer MVP with:
- Go API server
- React frontend embedded into one Go binary

## Architecture

Uploads are sent straight to object storage with a presigned `PUT`, but the zip
is only a transport container. A single in-process worker downloads each staged
zip sequentially, unpacks it entry by entry, and turns every image into a
content-addressed blob:

1. `POST /api/albums` registers a `QUEUED` album in SQLite and returns a
   presigned `PUT` URL for `uploads/<albumId>/source.zip`.
2. The browser uploads the zip directly to S3.
3. `POST /api/albums/<albumId>/finalize` queues the album for the pipeline
   worker.
4. The worker downloads the zip to local disk and walks its image entries in
   filename order (case-insensitive, so photo indexes stay stable). For each
   entry it:
   - reads the image dimensions and computes the SHA-256 of the raw bytes,
   - stores the original bytes in S3 at `blobs/<sha256>` (uploaded only once per
     distinct content hash, so identical images never occupy storage twice),
   - records the zip entry name, hash, width, height and ratio in SQLite,
   - computes an embedding and writes it to the blob row, unless that blob is
     already embedded.
5. After a successful extraction the staged zip is deleted from S3. On failure
   the zip is left in place and the album is marked `FAILED`, so
   `POST /api/albums/<albumId>/finalize` can retry it.

All metadata lives in the local SQLite catalog at `$STATE_DIR/viewer.db`;
S3 only holds the staged zip (briefly) and the deduplicated image blobs. The
`albums/<id>/index.json` objects of previous versions are no longer written or
read. Object keys below are the logical ones: set `S3_PREFIX` to nest all of
them under a single prefix in the bucket.

Image requests are resolved as `(albumId, index)` -> photo row -> blob hash ->
`blobs/<hash>`, with a local disk cache in `$STATE_DIR/images`. The cached
file is served through `http.ServeContent`, so responses carry a
`Content-Length`, support `Range`, and are revalidated with an `ETag` equal to
the blob hash under `Cache-Control: public, max-age=86400, immutable`.

## Configuration

The viewer is always deployed as the Docker image, so the whole configuration is
eight environment variables:

- `S3_ENDPOINT`, `S3_BUCKET`, `S3_ACCESS_KEY`, `S3_SECRET_KEY` — required.
- `S3_PREFIX` — optional key prefix, so several deployments can share one bucket
  (default empty: objects sit at the bucket root).
- `S3_USE_PATH_STYLE` — how the bucket is addressed: `true` (the default) puts it
  in the request path, `false` in a subdomain of the endpoint.
- `STATE_DIR` — directory holding the SQLite catalog and the disk caches
  (default `/tmp/viewer-cache`). It must be an absolute path.
- `PORT` — the port the container listens on (default `8080`).

By default the bucket is the first path segment of the request rather than a
subdomain — `https://host/bucket/key` instead of `https://bucket.host/key` —
which is what self-hosted stores like Garage and MinIO expect and needs no
wildcard DNS. An endpoint that itself carries a path prefix keeps it:
`https://host/gateway/bucket/key`. Set `S3_USE_PATH_STYLE=false` for a store that
requires the subdomain form, which then needs wildcard DNS for the endpoint. The
signing region is fixed at `us-east-1` because these stores ignore it.

`STATE_DIR` holds three things: `viewer.db` (the SQLite catalog), `images/`
(decoded blobs) and `zips/` (a staged upload while it is unpacked). The two
caches are disposable; the catalog is not, so mount a volume at `STATE_DIR` to
keep albums across container replacements. One volume covers all three because
every path lives directly under it.

`S3_PREFIX=photos` stores this deployment's objects under `photos/`
(`photos/blobs/<sha256>`, `photos/uploads/<albumId>/source.zip`,
`photos/batch/<name>.zip`). Surrounding slashes and whitespace are trimmed, so
`photos`, `photos/` and `/photos/` are equivalent. The prefix is applied by the
storage layer and never recorded in the catalog, so relocating a deployment's
objects within the bucket does not invalidate the stored metadata.

Setting the prefix is not a migration: a deployment that already has objects at
the bucket root must move its `blobs/`, `uploads/` and `batch/` keys under the
new prefix, or the viewer will not see them.

Everything else is a constant in `internal/config`: the signing region, the 1 GiB
upload limit, the 15 minute presign TTL, and the `/app/siglip2` checkpoint
location. Copy `.env.example` to `.env` for a local run. The test suite uses no
credentials, so `make test` needs no environment file.

There is no separate inference service to run: `viewer` loads the checkpoint
from `/app/siglip2` once at startup and runs the vision tower in-process. The
tower needs no network access at startup, and a checkpoint that fails to load
only degrades: the container logs the failure, keeps serving, and returns
recommendations from whatever embeddings the catalog already holds. Mount a
directory at `/app/siglip2` (holding `config.json` and `model.safetensors`) to
supply a checkpoint to `runtime-slim`.

Embeddings are stored as little-endian `float32` blobs on each image blob row in
SQLite. The ingest pipeline embeds new blobs inline; a blob whose embedding was
deferred (the model had not loaded) stays in the `pending` state, and the
background embedding workers fill it in on a later run once a checkpoint is
available. Those workers only start when the checkpoint loaded, so a server
without a model never computes embeddings but still answers recommendation
requests from the embeddings the catalog already holds. Recommendation responses
are cross-album only: photos from the same album as the query are excluded from
results. If no cross-album neighbors exist for an embedded query photo,
recommendations return an empty `items` list.

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
- `make build` compiles `bin/viewer`, rebuilding frontend assets when their sources are newer than the committed `internal/web/static` bundle (`make build FORCE=1` forces a frontend rebuild).
- `make test` runs the Go unit/integration tests (`go test ./cmd/... ./internal/...`). It needs no credentials or environment file.
- `make run` starts `bin/viewer` (loads `.env` if present, does not rebuild binaries).
- `make clean` removes build outputs, dependency caches, and the host-side `/tmp/viewer-cache` state directory.

## Batch ingest
- Any top-level `batch/*.zip` object is copied to `uploads/<albumId>/source.zip` and queued for extraction; the batch object is then removed. The `albumId` is derived from the object's ETag and size, so re-uploading identical bytes is deduplicated.

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
- Startup warmup loads the SQLite catalog into the recommendation index, scans the `batch/` prefix, and enqueues any album whose staged zip is still waiting; the embedding workers start once warmup finishes.
- `/metrics` exposes `viewer_embedding_images_total`, `viewer_embedding_images_ready`, `viewer_embedding_images_failed`, `viewer_embedding_images_pending` and `viewer_embedding_progress_ratio`, computed from the blob embedding statuses in the SQLite catalog.
- Requests that end in a 5xx are logged with the method, path, raw query, request ID (Chi request ID middleware), remote address and the internal error message; the JSON error body carries the code and message.
- Panics are logged with stack traces before the 500 response is returned.

## Known gaps
- The wall serves original-resolution images. There is no thumbnail or resizing support, and `/api/image/{albumId}/{index}` ignores every query parameter and returns the original bytes. This is deliberate for now, but a wall of many large photos moves a lot of bytes.
- There is no frontend typecheck gate: `vite build` (and therefore `make build` and the Docker build) does not run `tsc`. `npx tsc --noEmit` currently reports three pre-existing errors — two in `UploadPage.tsx` around `onRetryFailedUploads` (a widened `status: string`) and one in `ViewerPage.tsx` where `album` is possibly null.
- The batch ingest scan runs once at startup, so a `batch/*.zip` object added while the server is already running is only picked up on the next start.
- The SQLite catalog defaults to `/tmp/viewer-cache`, and the Dockerfile declares no volume for it. Replacing the container without a host mount at `STATE_DIR` leaves the blobs in S3 but loses the album/photo mapping, and the staged zips are already deleted, so albums cannot be reconstructed from blobs alone.
