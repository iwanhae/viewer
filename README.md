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
   presigned `PUT` URL for `uploads/<albumId>.zip`.
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
5. Once the worker has emptied its queue, the backup finalizer snapshots the
   SQLite catalog to S3 (`backups/viewer.db`) and only then batch-deletes the
   staged zips that snapshot covers — a failed extraction's zip included, since
   the failure is now recorded in the catalog.

All metadata lives in the local SQLite catalog at `$STATE_DIR/viewer.db`
(default `/var/lib/viewer`); S3 holds the staged zips (until the next drain),
the deduplicated image blobs, and the catalog backup. The
`albums/<id>/index.json` objects of previous versions are no longer written or
read. Object keys below are the logical ones: set `S3_PREFIX` to nest all of
them under a single prefix in the bucket.

Image requests are resolved as `(albumId, index)` -> photo row -> blob hash ->
`blobs/<hash>`. The blob is fetched from S3 per request and served through
`http.ServeContent`, so responses carry a `Content-Length`, support `Range`, and
are revalidated with an `ETag` equal to the blob hash under
`Cache-Control: public, max-age=86400, immutable` — a repeat view is a `304`
the browser answers without reaching the server.

`uploads/` is also the drop zone: at startup and every minute the viewer lists
it, registers an album for every zip nothing owns yet, and queues an album whose
zip is still waiting. A zip copied in with `mc`/`aws s3 cp` is therefore picked
up without a restart, and any zip left behind by an interrupted extraction is
retried. The scan never copies, moves or deletes: the backup finalizer empties
the prefix in batches once a drain has backed the catalog up.

The catalog itself is backed up to the bucket, and the bucket restores the
catalog: at startup the viewer compares `backups/viewer.db`'s timestamp with
`viewer.db.backup-stamp` in `STATE_DIR` and replaces the local database when
the bucket is ahead — including when the local file is missing entirely, which
is what makes a wiped state volume recoverable from the bucket alone.

## Configuration

The viewer is always deployed as the Docker image, so the whole configuration is
eight environment variables:

- `S3_ENDPOINT`, `S3_BUCKET`, `S3_ACCESS_KEY`, `S3_SECRET_KEY` — required.
- `S3_PREFIX` — optional key prefix, so several deployments can share one bucket
  (default empty: objects sit at the bucket root).
- `S3_USE_PATH_STYLE` — how the bucket is addressed: `true` (the default) puts it
  in the request path, `false` in a subdomain of the endpoint.
- `STATE_DIR` — absolute directory holding the SQLite catalog (default
  `/var/lib/viewer`, the volume the image declares).
- `PORT` — the port the container listens on (default `8080`).

By default the bucket is the first path segment of the request rather than a
subdomain — `https://host/bucket/key` instead of `https://bucket.host/key` —
which is what self-hosted stores like Garage and MinIO expect and needs no
wildcard DNS. An endpoint that itself carries a path prefix keeps it:
`https://host/gateway/bucket/key`. Set `S3_USE_PATH_STYLE=false` for a store that
requires the subdomain form, which then needs wildcard DNS for the endpoint. The
signing region is fixed at `us-east-1` because these stores ignore it.

`STATE_DIR` holds `viewer.db`, the SQLite catalog, plus the backup stamp that
records which bucket backup it reflects. The catalog is the only state that
cannot be rebuilt from the blobs — the album-to-photo mapping exists nowhere
else — which is exactly why the viewer keeps a snapshot of it at
`backups/viewer.db` in the bucket: losing the volume costs one restart, not the
library. The image Dockerfile declares `VOLUME /var/lib/viewer`; mount a host
volume there (or point `STATE_DIR` at your own mount) to keep albums across
container replacements.

The viewer keeps no local caches: images stream straight from S3, and the only
thing it writes outside the volume is the staged zip being unpacked, which goes
to the OS temp directory. Startup clears that directory of leftovers from a
crashed run, so a container replacement or a restart leaves nothing to clean up
by hand.

`S3_PREFIX=photos` stores this deployment's objects under `photos/`
(`photos/blobs/<sha256>`, `photos/uploads/<albumId>.zip`). Surrounding slashes
and whitespace are trimmed, so `photos`, `photos/` and `/photos/` are
equivalent. The prefix is applied by the storage layer and never recorded in the
catalog, so relocating a deployment's objects within the bucket does not
invalidate the stored metadata.

Setting the prefix is not a migration: a deployment that already has objects at
the bucket root must move its `blobs/` and `uploads/` keys under the new prefix,
or the viewer will not see them.

Everything else is a constant in `internal/config`: the signing region, the 1 GiB
upload limit, the 15 minute presign TTL, the `/app/siglip2` checkpoint
location, and the `SIGLIP2_MODEL_URL` mirror the checkpoint is fetched from.
Copy `.env.example` to `.env` for a local run. The test suite uses no
credentials, so `make test` needs no environment file.

There is no separate inference service to run: `viewer` loads the checkpoint
from `/app/siglip2` and runs the vision tower in-process. The server starts
listening right away, and the checkpoint resolves in a background goroutine: a
cold start fetches it into that directory from `SIGLIP2_MODEL_URL`, a
public-read mirror of the upstream Hugging Face files, logging position and
rate every 5 seconds while the download runs. Until the model is in, the API
answers recommendations from whatever embeddings the catalog already holds and
new blobs stay `pending`. A directory that already holds a checkpoint - a
previous download or a mount - is never re-fetched. A checkpoint that fails to
load or download only degrades: the container logs the failure, keeps serving,
and returns recommendations from whatever embeddings the catalog already holds.
Mount a directory at `/app/siglip2` (holding `config.json` and
`model.safetensors`) to supply a checkpoint by hand and skip the download
entirely.

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

### Embedding progress

Indexing and embedding are two stages, and the album status only covers the
first: an album is `SUCCEEDED` while its embeddings may still be running. Both
stages are reported:

- `GET /api/albums/<albumId>/finalize` carries an `embedding` object covering
  that album's distinct blobs, so the upload page can show an album that is
  indexed but not yet fully embedded.
- `GET /api/embedding` reports the same shape for the whole catalog; the wall
  polls it and shows an `Embedding <ready>/<total>` indicator while work is
  left.

```json
{"enabled":true,"active":true,"total":300,"ready":181,"failed":2,"pending":117,"ratio":0.603333}
```

`enabled` is false when no checkpoint could be loaded - the fetch failed, or
the files are missing. Nothing is embedding then and nothing ever will be, so
a client stops waiting instead of showing a bar that cannot move; the blobs
simply stay `pending` until a deployment with a model picks them up. During
the cold-start download window `enabled` is already true while nothing is
active yet: the workers come up when the checkpoint finishes loading. `failed` images are terminal — the retry worker skips them — so
`ratio` reaches 1 only when nothing is pending, and a failure is visible as
`ready + failed == total` instead of a bar that never fills.

Docker:
- `docker build .` produces one image: the Go viewer and the frontend assets.
  It carries no checkpoint - a cold start downloads it (~1.5 GiB) into
  `/app/siglip2` from `SIGLIP2_MODEL_URL` in the background while the server
  is already serving, so the published image stays small.
  Mount a volume at `/app/siglip2` to keep that download across container
  replacements, or set `SIGLIP2_MODEL_URL` to mirror a different model.
- CI publishes the viewer as `ghcr.io/<owner>/<repo>`.

Only the first start needs egress to the mirror - the same host the viewer
already talks to for object storage - and never to Hugging Face. A download
that fails leaves no partial file behind: the next start retries, and in the
meantime the viewer degrades to serving without embeddings rather than
crashing.

## Commands
- `make build` compiles `bin/viewer`, rebuilding frontend assets when their sources are newer than the committed `internal/web/static` bundle (`make build FORCE=1` forces a frontend rebuild).
- `make test` runs the Go unit/integration tests (`go test ./cmd/... ./internal/...`). It needs no credentials or environment file.
- `make typecheck` runs the frontend TypeScript check without producing a bundle.
- `make run` starts `bin/viewer` (loads `.env` if present, does not rebuild binaries). Set `STATE_DIR` to a writable directory: the container default `/var/lib/viewer` is not writable for a local user.
- `make clean` removes build outputs and dependency caches. The viewer itself keeps no caches: images stream from S3 and staged zips live in the OS temp directory.

## Upload drop zone
- Whatever sits under `uploads/` is ingested: at startup and every minute the viewer lists the prefix, and for each zip it finds, it either queues the album that owns it again (its status is `QUEUED` or `PROCESSING`, so no worker holds it) or registers a new album and queues that. `albumId` is derived from the object's ETag and size, so dropping identical bytes twice resolves to one album.
- Nothing is copied, moved or deleted by the scan. A finished album's zip is skipped and stays until the next drain-time batch delete, so a `SUCCEEDED` album with its zip still present is the normal state between a drain and the finalize that follows it.

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
- Embedding is reported as it happens. The background workers log
  `recommend: embedding run started pending=<n> ready=<n> total=<n>`, one
  `recommend: embedded blob=<sha256> bytes=<n> dim=768 elapsed=<d>` line per
  image, a `recommend: embedding progress ready=<n>/<n> pending=<n> failed=<n>`
  line every 25 images, and finally
  `recommend: embedding run finished embedded=<n> ready=<n>/<n> pending=<n> failed=<n> duration=<d> avg=<d>`.
  A drain that keeps finding work stays one run, so the summaries mark real
  start and end points rather than one per batch.
- Ingest-time embeddings, which the pipeline computes inline, log
  `pipeline: album=<id> extracting entries=<n>`, then
  `pipeline: album=<id> blob=<sha256> embedded bytes=<n> dim=768 elapsed=<d>`
  per image, and end with
  `pipeline: album=<id> ready photos=<n> embedded=<n> pending=<n> failed=<n>`, so
  an album that could not be embedded says so on its own line.
- The upload prefix is scanned at startup and then once a minute. Each scan logs
  `upload ingest: prefix=uploads/ listed=<n> candidates=<n>`, one line per zip it
  registers or requeues, and
  `upload ingest: scan finished discovered=<n> registered=<n> requeued=<n> skipped=<n> errors=<n> duration=<d>`.
  A failed scan is logged and retried on the next tick instead of ending the
  watch.
- Startup warmup loads the SQLite catalog into the recommendation index and
  logs `catalog warmup started` / `catalog warmup finished duration=<d>`; the
  upload scan and then the embedding workers start once it finishes.
- `/metrics` exposes `viewer_embedding_images_total`, `viewer_embedding_images_ready`, `viewer_embedding_images_failed`, `viewer_embedding_images_pending` and `viewer_embedding_progress_ratio`, computed from the blob embedding statuses in the SQLite catalog.
- Requests that end in a 5xx are logged with the method, path, raw query, request ID (Chi request ID middleware), remote address and the internal error message; the JSON error body carries the code and message.
- Panics are logged with stack traces before the 500 response is returned.

## Known gaps
- The wall still serves original-resolution images. Scaled variants exist on the image endpoint for the places that need them — `GET /api/image/{albumId}/{index}?w=<320|640|1024>` resamples the blob to that width as a JPEG (an original already no wider than the request passes through untouched, and the scaled width joins the blob hash in the ETag) — but the wall does not use it yet, so a wall of many large photos still moves a lot of bytes.
- `GET /api/albums/search` matches the query anywhere in the lowercased original filename (not just as a prefix) and each result carries a `cover` — the photo at index 0 with its dimensions — so the Find Albums page can render cover cards at `w=640` without a second request per album.
- The upload scan adopts zips but never advertises itself: files dropped into the bucket from outside appear in the library without ever passing through the upload page. It also adopts a zip mid-library the moment it appears, with no way to park one aside.
- Losing the state volume no longer loses the library — the bucket holds `backups/viewer.db` and startup restores it — but the backup lands only when the extraction queue drains, so anything uploaded since the last drain exists in exactly two places: the bucket's blobs and the local catalog. Backups of a deployment that never drains wait for its first drain.

## Frontend
- The bundle ships its own Space Grotesk (via fontsource, no CDN) over a design-token layer in `src/styles/tokens.css`; base primitives (focus ring, progress, skeleton, reduced-motion guards) live in `src/styles/base.css`, and the Find Albums and Upload pages own their styles next to their components.
- `npm --prefix frontend run typecheck` runs `tsc --noEmit`; it gates `npm run build`, `make build` and the Docker build, so a type error fails the build instead of shipping.
- Every reload revalidates `index.html` (`Cache-Control: no-cache`); the hashed files under `/assets/` are served `immutable`, so only a redeploy changes what the browser keeps.
