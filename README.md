# viewer

Self-hosted photo album server. Upload a zip of photos; the server stores each image under its original SHA-256 in S3-compatible object storage, catalogs albums in SQLite, computes SigLIP2 image embeddings into an optional external Qdrant server, and serves a React frontend plus a JSON API from a single Go binary. Without Qdrant everything works except the "similar photos" recommendations, which answer with a 503.

## Features

- Zip batch upload via presigned PUT (1 GiB cap).
- Deduplicated storage: objects live at `blobs/<original-sha256>`, so identical uploaded images are stored once even after optional WebP encoding.
- On-the-fly resized JPEGs (320/640/1024 widths), streamed from S3 and cached with response-byte ETags.
- Optional external WebP encoder using `cwebp` Q85, preserving supported EXIF/ICC/XMP metadata and replacing only when smaller.
- Semantic "similar photos" recommendations across albums (SigLIP2-base, 768-dim vectors in Qdrant).
- Lease-based HTTP API so external GPU workers can drain the embedding backlog.
- Protected admin browser for catalog, photo, blob, and album pipeline inspection; existing stats and recovery actions remain at `/admin/legacy`.
- SQLite catalog backed up to the bucket and auto-restored on startup.
- Upload drop zone: zips copied straight into the `uploads/` prefix (e.g. with `mc` or `aws s3 cp`) are picked up without a restart.

## Architecture

Object keys live under `uploads/` (staged zips), `blobs/` (image payloads), and `backups/` (catalog snapshots). The `encoding/` object prefix is retained only as a legacy compatibility path during the mixed-worker rollout; new encoder workers stream results to the API instead. The API spools each result under `$STATE_DIR/encoding-spool`, then bounded background processing validates it and conditionally writes the accepted WebP to S3. Catalog metadata and queue state live in SQLite.

1. `POST /api/albums` registers a QUEUED album and returns a presigned PUT for `uploads/<albumId>.zip`; the browser uploads the zip straight to object storage.
2. `POST /api/albums/<id>/finalize` queues extraction.
3. A single-slot worker downloads the zip and stores every image as `blobs/<sha256>`, recording entry name, hash, width, and height in SQLite.
4. External WebP encoder workers stream results back to the API. Results are spooled under `$STATE_DIR/encoding-spool`, asynchronously validated, and conditionally written to the original `blobs/<sha256>` key; only then is encoding marked complete. The `encoding/` object prefix remains available only for legacy workers during the mixed-worker rollout.
5. Embedding workers — the built-in GoMLX one in-process, plus optional external ones — embed pending blobs and write vectors to Qdrant; the catalog keeps only embedding status.
6. Image requests resolve `(albumId, index)` to a blob hash and stream `blobs/<hash>` from S3, served with a response-byte `ETag` and `Cache-Control: public, max-age=86400`.

Metadata lives in SQLite at `$STATE_DIR/viewer.db`. The bucket snapshot `backups/viewer.db` is restored at startup when it is newer than the local catalog, so a wiped state volume recovers from the bucket alone. The SigLIP2 checkpoint is fetched at first start into `/app/siglip2`; inference runs in-process, with no separate inference service. A cold start degrades gracefully: the server serves, embeddings wait.

## Requirements

- Go 1.27
- Node 22 (frontend build)
- Docker (alternative)

Tests need no credentials.

## Getting started

Local run — point `STATE_DIR` at a writable directory, since the container default `/var/lib/viewer` is not writable for a local user:

```sh
cp .env.example .env   # fill in the required variables
make build
make run               # loads ./.env if present
```

Docker:

```sh
docker build -t viewer .
# The three QDRANT_* variables are optional: without them the viewer runs
# with photo recommendations disabled and everything else working.
docker run -d --name viewer \
  -p 8080:8080 \
  -v viewer-state:/var/lib/viewer \
  -v viewer-model:/app/siglip2 \
  -e S3_ENDPOINT=https://s3.example.com \
  -e S3_BUCKET=viewer \
  -e S3_ACCESS_KEY=replace_me \
  -e S3_SECRET_KEY=replace_me \
  -e QDRANT_URL=https://qdrant.example.com \
  -e QDRANT_API_KEY=replace_me \
  -e QDRANT_COLLECTION=photo_embeddings \
  viewer
```

`viewer-state` holds the SQLite catalog and must persist: the album-to-photo mapping cannot be rebuilt from the blobs. `viewer-model` keeps the ~1.5 GiB model checkpoint across container replacements. CI publishes the image to `ghcr.io/<owner>/<repo>`.

## Configuration

The viewer is deployed as a Docker image, and every setting is an environment variable. `.env.example` documents the same set.

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `S3_ENDPOINT` | yes | — | Object storage endpoint (S3, MinIO, Garage, ...). Any path prefix on the endpoint is kept. |
| `S3_BUCKET` | yes | — | Bucket name. |
| `S3_ACCESS_KEY` | yes | — | Access key. |
| `S3_SECRET_KEY` | yes | — | Secret key. |
| `S3_PREFIX` | no | (empty) | Key prefix so several deployments can share one bucket; surrounding slashes are trimmed. Objects become `<prefix>/uploads/...`, `<prefix>/blobs/...`, `<prefix>/backups/...`. |
| `S3_USE_PATH_STYLE` | no | `true` | `true` addresses the bucket in the request path (`https://host/bucket/key` — what self-hosted stores expect); `false` uses a subdomain (`https://bucket.host/key`, needs wildcard DNS). |
| `STATE_DIR` | no | `/var/lib/viewer` | Absolute directory holding the SQLite catalog (`viewer.db`), backup stamp, and a bounded encoder spool (`encoding-spool/`, up to 2 GiB). Must be an absolute path. Mount a volume here — the album-to-photo mapping cannot be rebuilt from the blobs. |
| `PORT` | no | `8080` | HTTP listen port. |
| `QDRANT_URL` | no | (empty) | Base URL of the Qdrant server's REST API; must be an http/https URL with a host. Setting it turns photo recommendations on; leaving it empty runs the viewer without a vector store. |
| `QDRANT_API_KEY` | no | (empty) | Sent as the `api-key` header on every Qdrant request. Sent only when set, so an unauthenticated server needs no placeholder. |
| `QDRANT_COLLECTION` | when `QDRANT_URL` is set | — | Qdrant collection holding the per-photo embedding points. Deliberately no default: the viewer refuses to start with a URL but no collection. |
| `SIGLIP2_MODEL_URL` | no | built-in mirror | Base URL the SigLIP2 checkpoint (`config.json`, `model.safetensors`, `tokenizer.json`) is fetched from on first start into `/app/siglip2`. Mount a prepared directory at `/app/siglip2` to skip the download. |
| `WORKER_TOKEN` | no | (empty) | Shared bearer token for the external recommender and encoder APIs. Setting it enables WebP encoding and gates new non-WebP image embeddings until encoding finishes. |
| `ADMIN_TOKEN` | no | (empty) | Basic-auth password for `/admin`. Empty keeps the admin UI disabled. |
| `ALLOW_BACKUP_OVERWRITE` | no | `false` | Lets the catalog finalizer overwrite the bucket's `backups/viewer.db` with a local database it would otherwise refuse to write (untraceable stamp, or drastically smaller than the backup it would replace). |

Notes:

- Set-but-blank values of `QDRANT_URL`, `WORKER_TOKEN`, `QDRANT_API_KEY`, `QDRANT_COLLECTION`, and `ADMIN_TOKEN` are rejected at startup rather than silently meaning "off".
- `WEBP_ENCODING_ENABLED` is no longer read. Setting `WORKER_TOKEN` enables WebP encoding; there is no separate opt-out.
- Existing deployments must rename `EMBEDDING_WORKER_TOKEN` to `WORKER_TOKEN`; a non-empty old variable now fails startup instead of leaving the worker API open.
- Similarly, `QDRANT_API_KEY` or `QDRANT_COLLECTION` without `QDRANT_URL` is rejected: Qdrant is optional, but half-configured is a mistake, not an opt-out.
- Some values are fixed constants, not environment variables: the 1 GiB upload cap, the 15 minute presign TTL, the `us-east-1` signing region, and the `/app/siglip2` checkpoint directory. See `internal/config/config.go`.

## API overview

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/healthz` | Liveness. |
| GET | `/metrics` | Prometheus metrics; the only exposure of embedding progress. |
| POST | `/api/albums` | Register an album, get a presigned upload URL. |
| POST | `/api/albums/{id}/finalize` | Queue extraction (`GET` returns current status). |
| GET | `/api/albums/{id}` | Album detail with photos. |
| GET | `/api/albums/search` | Filename substring search. |
| GET | `/api/feed` | Paged photo wall. |
| GET | `/api/image/{hash}` | Blob content; `?w=320\|640\|1024` returns a resized JPEG. ETags identify the returned bytes. |
| GET | `/api/recommendations/{albumId}/{index}` | Cross-album similar photos. |
| GET | `/api/photos/search?q=&limit=` | Best photo per album for a natural-language description; requires `QDRANT_URL`. |
| POST | `/api/photos/search-by-image` | Best photo per album for an uploaded picture; requires `QDRANT_URL`. |
| POST | `/api/embedding/claim`, `/renew`, `/results` | External embedding-worker lease API (bearer-token when `WORKER_TOKEN` is set). |
| POST | `/api/encoding/claim`, `/renew`, `/complete`, `/output` | External WebP encoder lease/result API, enabled when `WORKER_TOKEN` is set. New workers request `outputMode: "api"`; result streams go to `/output`, are spooled under `$STATE_DIR/encoding-spool`, and are asynchronously validated before a conditional S3 write. Claims without that mode retain the legacy presigned-PUT contract during rollout. |
| GET | `/admin` | Independent React admin browser; enabled only when `ADMIN_TOKEN` is set. |
| GET | `/admin/legacy` | Existing operational stats and embedding/encoding recovery dashboard, preserved during the admin UI rollout. |
| GET | `/admin/api/overview` | Catalog-only pipeline totals and normalized/raw state breakdowns. |
| GET | `/admin/api/photos` | Paged photo rows with album metadata and current encoding/embedding state. |
| GET | `/admin/api/photos/{albumId}/{idx}` | One photo row with current pipeline state. |
| GET | `/admin/api/blobs` | Paged blob state and reference counts, including unreferenced blobs. |
| GET | `/admin/api/blobs/{hash}` | One blob with relation counts; fetch references through the paged photo list. |
| GET | `/admin/api/albums` | Paged album processing state, error, and actual photo-row count. |
| GET | `/admin/api/albums/{id}` | Album processing state and overview-equivalent counts for its referenced blobs/photos. |

The admin document, legacy dashboard, and resource APIs require the same HTTP
Basic password (`ADMIN_TOKEN`), and are absent when the admin service or token
is disabled. Vite bundles are static code served by the existing asset pipeline
and may be fetched from `/assets` without authentication; they contain no
catalog data or credentials. The React admin entry is separate from the viewer
SPA and uses isolated styles and routes. List
responses use `{photos|blobs|albums, nextCursor}`; `nextCursor` is `null` at the
end. `limit` defaults to 50 and accepts 1–100. Cursors are URL-safe opaque
keyset positions bound to the resource, filters, and limit; send the same
filters and limit on continuation requests. Pages are ordered by
`(albumId, idx)`, `hash`, or `id`, respectively. They are separate reads, not a
snapshot across concurrent catalog updates. Unknown query parameters, repeated
parameters, invalid categories/statuses, and malformed or mismatched cursors
return 400. Missing detail resources return 404.

`GET /admin/api/photos` accepts `q`, `albumId`, `hash`, `encoding`, `embedding`,
`encodingRaw`, `embeddingRaw`, `skippedReason`, `limit`, and `cursor`. `q` is a
ASCII-case-insensitive literal substring of the album filename or photo name (`%`,
`_`, and `\` are ordinary characters). `hash` is exact. `encoding` accepts the
overview categories `queued`, `processing`, `validating`, `committing`,
`completed`, `skipped`, `failed`, and `unknown`; `embedding` accepts `queued`,
`blocked`, `processing`, `completed`, `failed`, and `unknown`. The `*Raw`
filters match exact stored states, including states this version does not
recognize. `skippedReason` is `webp` or `nonWebp`, based only on stored content
type. `q` is limited to 256 bytes and either raw-state filter to 128 bytes.
Multiple filters combine with AND. Photo rows include the current raw and
normalized states, error text, and `blockReason: "encoding_gate"` only for
pending embeddings that fail the same gate used by embedding claims.
If `missingBlob` is true, the photo row's hash has no blob row: `sizeBytes` and
`contentType` are `null`, and pipeline state/error fields are empty rather than
being classified as an unknown blob.

`GET /admin/api/blobs` accepts `albumId`, exact `hash`, the same encoding,
embedding, raw-state, and skipped-reason filters, plus `unreferenced=true|false`,
`limit`, and `cursor`. Blob details include `photoCount` and distinct
`albumCount`, not embedded reference arrays. Use
`GET /admin/api/photos?hash={hash}` to page through photos for a shared blob.
`GET /admin/api/albums` accepts a literal-substring `q`, `status` (`QUEUED`,
`PROCESSING`, `SUCCEEDED`, or `FAILED`), `limit`, and `cursor`. Album detail
pipeline breakdowns use the same category/raw-state meanings and count units as
the overview, scoped to that album; its referenced photos are available through
`GET /admin/api/photos?albumId={id}`.
`library.photos` and album `photoCount` include every photo row, while pipeline
category photo counts include only rows with a blob. Therefore the encoding or
embedding category `photoCount` sum plus `missingBlobPhotos` equals the total
photo count (assuming photo rows reference existing albums). The overview's
`library.missingBlobPhotos` and album detail's `missingBlobPhotos` are counted
in their respective catalog read transactions.

The browse queries use existing primary-key and hash indexes and add no schema
migration. Literal substring searches use SQLite `instr(lower(...))` and are
not indexed; state-filtered photo/blob pages can also examine many rows on a
large catalog. The current indexes support ordered album/photo/hash traversal,
but production `EXPLAIN QUERY PLAN` and workload measurements should guide any
future approved index migration. No list API reports retry counts or invented
retry/timing estimates.

For an encoder rollout, deploy the API server before upgrading workers; old
workers remain supported by the legacy claim mode. Do not roll back to a server
version that predates API output uploads while catalog jobs are in `received`
or `committing`: that version cannot finish those server-owned jobs. The API
accepts at most 1 GiB per result, and retains a bounded two-result spool on the
state volume. A `202` confirms a local handoff, not a completed S3
replacement; if the state volume is lost, missing results are re-queued.

## External embedding workers

The built-in embedder is a single in-process goroutine. The same claim/renew/results lease API is open to external workers, so a fleet of GPU boxes can drain a large backlog: claims lease pending blobs, results write vectors to Qdrant and then status to SQLite, and expired leases keep blobs from being stranded. See `worker/README.md` for the Python workers (`uv run recommender` and `uv run encoder`).

## Development

- `make build` — builds the frontend if stale (`FORCE=1` to force), then `bin/viewer`.
- `make test` — Go unit and integration tests; no credentials or env file needed.
- `make frontend-test` — React admin UI tests (Vitest + Testing Library).
- `make typecheck` — frontend `tsc --noEmit`.
- `make run` — runs `bin/viewer` with `./.env`.
- `make clean`.

The Go SigLIP2 port (`internal/vision`) is regression-tested against a PyTorch reference in `scripts/vision-reference/`; that test is skipped unless `VISION_REFERENCE_DIR` points at a generated bundle.

## Project structure

```
cmd/viewer/                entry point
internal/                  API, catalog, pipelines, vision, storage — see package docs for detail
frontend/                  React + TypeScript + Vite viewer SPA and independent admin entry, built into internal/web/static and embedded
worker/                   Python recommender and WebP encoder workers
scripts/vision-reference/  PyTorch reference model for the vision-port test
```
