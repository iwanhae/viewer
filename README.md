# viewer

Self-hosted photo album server. Upload a zip of photos; the server stores each image under its original SHA-256 in S3-compatible object storage, catalogs albums in SQLite, computes SigLIP2 image embeddings into an optional external Qdrant server, and serves a React frontend plus a JSON API from a single Go binary. Without Qdrant everything works except the "similar photos" recommendations, which answer with a 503.

## Features

- Zip batch upload via presigned PUT (1 GiB cap).
- Deduplicated storage: objects live at `blobs/<original-sha256>`, so identical uploaded images are stored once even after optional WebP encoding.
- On-the-fly resized JPEGs (320/640/1024 widths), streamed from S3 and cached with response-byte ETags.
- Admin-controlled local and optional external WebP encoding using `cwebp` Q85, preserving supported EXIF/ICC/XMP metadata and replacing only when smaller.
- Semantic "similar photos" recommendations across albums (SigLIP2-base, 768-dim vectors in Qdrant).
- Lease-based HTTP API so external GPU workers can drain the embedding backlog.
- Protected admin browser for pipeline inspection, with confirm-gated synchronous recovery and local execution control.
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
| `WORKER_TOKEN` | no | (empty) | Shared bearer token for external embedding and encoder APIs. Setting it enables external encoding routes; local encoding and spool recovery are always available. |
| `ADMIN_TOKEN` | no | (empty) | Basic-auth password for `/admin`. Empty keeps the admin UI disabled. |
| `ALLOW_BACKUP_OVERWRITE` | no | `false` | Lets the catalog finalizer overwrite the bucket's `backups/viewer.db` with a local database it would otherwise refuse to write (untraceable stamp, or drastically smaller than the backup it would replace). |

Notes:

- Set-but-blank values of `QDRANT_URL`, `WORKER_TOKEN`, `QDRANT_API_KEY`, `QDRANT_COLLECTION`, and `ADMIN_TOKEN` are rejected at startup rather than silently meaning "off".
- `WEBP_ENCODING_ENABLED` is no longer read. `WORKER_TOKEN` enables only the external encoding API, not local capability.
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
| GET | `/admin/api/overview` | Catalog inventory totals, unique-blob pipeline breakdowns, and a health section: expired leases, integrity counts (unreferenced blobs, blob-less photos), feature flags, and a best-effort vector-store count comparison that degrades instead of failing. |
| GET | `/admin/api/issues` | Bounded groups of current pipeline/archive failures and latest-run failed extraction entries. |
| GET | `/admin/api/photos` | Paged photo rows with album metadata and current encoding/embedding state. |
| GET | `/admin/api/photos/{albumId}/{idx}` | One photo row with current pipeline state. |
| GET | `/admin/api/blobs/{hash}` | One blob with relation counts and current per-stage observations; fetch references through the paged photo list. |
| GET | `/admin/api/blobs/{hash}/history` | Bounded, cursor-paged stage attempt and major-transition history. |
| GET | `/admin/api/albums` | Paged album processing state, error, and actual photo-row count. |
| GET | `/admin/api/albums/{id}` | Album processing state, error, photo and missing-blob-photo counts, and the latest extraction run summary. |
| GET | `/admin/api/extraction-failures` | Cursor-paged failed entries from each album's latest run when terminal, filtered by `reasonCode`. |
| GET / POST | `/admin/api/automatic/embedding` | Read / change automatic local embedding; POST body `{"enabled":true}` or `{"enabled":false}`. |
| POST | `/admin/api/encoding/run-local` | Start one background local WebP encoding drain over the pending non-WebP blobs; body `{}`. |
| POST | `/admin/api/reindex` | Re-embed every ready blob; answers with the fresh embedding counts. |
| POST | `/admin/api/vector-sync` | Diff the catalog's expected points against the vector store and re-embed only the blobs whose points are missing; orphans are counted, not deleted. Synchronous (tens of seconds); body `{}`. |
| POST | `/admin/api/recover` | Run one synchronous recovery target, optionally narrowed by a `hashes` list. |

For an encoder rollout, deploy the API server before upgrading workers; old
workers remain supported by the legacy claim mode. Do not roll back to a server
version that predates API output uploads while catalog jobs are in `received`
or `committing`: that version cannot finish those server-owned jobs. The API
accepts at most 1 GiB per result, and retains a bounded two-result spool on the
state volume. A `202` confirms a local handoff, not a completed S3
replacement; if the state volume is lost, missing results are re-queued.

## Admin

`/admin` is an independent React browser with its own entry, styles, and routes, registered only when `ADMIN_TOKEN` is set. Everything under `/admin` — pages and API alike — requires HTTP Basic auth with the token as the password; without the token the whole surface answers 404. Basic authentication protects admin mutations as well as reads. The bundled JS under `/admin/assets` is static code served without auth; it contains no catalog data or credentials.

### Pages

- **개요** (Overview) — library totals (albums, photos, unique blobs); encoding and embedding category counts over unique blobs, each linking into the photo list; the 시스템 상태 health panel (expired encoding/embedding leases, unreferenced blobs, blob-less photos, a best-effort vector-store count comparison that degrades instead of failing, and feature flags that report server configuration, never worker liveness); the 복구 작업 panel of confirm-gated recovery actions; and the 실행 제어 panel.
- **사진** (Photos) — the list browser for photo rows. The filter form exposes `q`, `albumId`, `hash`, `encoding`, and `embedding`; the URL-only filters below arrive as deep links and are shown as a notice. Photo detail shows per-stage state, observation, and error, plus per-blob recovery buttons.
- **블롭 상세** (Blob detail) — reachable through hash links only; there is no blob list page. It shows stored content, per-stage state and observation, the newest 20 pipeline events, references, and per-blob recovery buttons.
- **앨범** (Albums) — the album list plus a slim detail page: status, error, photo and missing-blob-photo counts, the latest extraction run summary, and drill links into the photo list.
- **이슈** (Issues) — current failure groups by stage and safe error code with drill-downs, plus the extraction-failures browser showing which ZIP entries failed.

### API

List endpoints answer `{photos|albums, nextCursor}`; `nextCursor` is `null` at the end. `limit` defaults to 50 and accepts 1–100. Cursors are URL-safe opaque keyset positions bound to the resource, filters, and limit, so continuation requests must repeat them. Pages are ordered by `(albumId, idx)` or `id` and are independent reads, not snapshots. Unknown or repeated query parameters, invalid categories, and malformed or mismatched cursors return 400; missing detail resources return 404.

- `GET /admin/api/photos` accepts the form filters `q`, `albumId`, `hash`, `encoding`, `embedding` plus the URL-only `encodingErrorCode`, `embeddingErrorCode`, `encodingLeaseExpired`, `embeddingLeaseExpired`, `missingBlob`, and `limit`/`cursor`. `q` is an ASCII-case-insensitive literal substring of the album filename or photo name (`%`, `_`, and `\` are ordinary characters; 256-byte cap; not indexed). `hash` is exact. `encoding` accepts the overview categories `queued`, `processing`, `validating`, `committing`, `completed`, `skipped`, `failed`, `unknown`; `embedding` accepts `queued`, `processing`, `completed`, `failed`, `unknown`. The error-code filters are exact and only match rows whose current corresponding stage is `failed`. Filters combine with AND. Photo rows carry raw and normalized states, error text and code, and the per-stage observation (`startedAt`, `finishedAt`, `statusChangedAt`, `attempts`, `phase`, `errorCode`, `retryability`). A `missingBlob` row has no blob: `sizeBytes` and `contentType` are `null` and the pipeline fields are empty.
- `GET /admin/api/blobs/{hash}` answers one blob with the same stage fields plus `photoCount` and distinct `albumCount`; page its references through `GET /admin/api/photos?hash=...`. `GET /admin/api/blobs/{hash}/history?limit=&cursor=` pages its pipeline events. Observations and history begin with migration 0006: existing rows are backfilled with their current status and no inferred timings, and failures that cannot be safely classified use `errorCode: "legacy_unknown"`. History keeps the last 90 days of structured event/status/error codes only — worker tokens, stage keys, signed URLs, and raw error text are never copied into it.
- `GET /admin/api/albums` accepts `q` (same substring rule), `status` (`QUEUED`, `PROCESSING`, `SUCCEEDED`, `FAILED`), `limit`, and `cursor`. Album detail answers status, error, `photoCount`, `missingBlobPhotos`, and the latest extraction run summary; `extractionRecorded: false` marks albums that predate run recording.
- `GET /admin/api/issues` groups current failures: encoding and embedding groups count unique blobs whose current stage status is `failed`, grouped by safe `errorCode`; current `FAILED` albums form one `album_extraction` group; failed entries from each album's latest terminal extraction run group by validated reason code. Counts use different units per stage (blobs, albums, entries) and are not additive. Groups are capped at 100, with `truncated` and `remainingGroups` reporting omissions. `GET /admin/api/extraction-failures?reasonCode=&limit=&cursor=` pages one group's entries, ordered by `(albumId, ordinal)`: entry names, album identity, run ID, ordinal, and the safe reason code — never raw error detail.

### Recovery and execution control

- `POST /admin/api/recover` runs exactly one synchronous target per request and answers with that action's typed result plus fresh counts. Targets: `release_expired_claims` (expired embedding claims back to pending; valid claims never move), `release_all_claims` (also live embedding leases — API-only, no UI button), `retry_failed_embeddings`, `retry_failed_encodings` (failed non-WebP conversions), and `reset_stuck_encoding` (expired encoding leases and uncommitted `received` results back to pending; `committing` rows are never touched). An optional `hashes` list of lowercase 64-hex SHA-256 digests (at most 100) narrows any target to those blobs; without it the target applies library-wide. Every action is a single synchronous status flip that the existing workers drain — there is no queue or background executor — and the response reports the rows actually moved.
- `POST /admin/api/reindex` flips every ready embedding back to pending — the costly global action — and answers with the fresh embedding counts.
- `POST /admin/api/vector-sync` diffs the catalog's expected points against the vector store and re-embeds only the blobs whose points are missing (orphan points are counted, not deleted); `started:false` means a pass was already running. The sync is count-consistent, not payload-consistent — a point whose vector is stale but present is not re-embedded — and a concurrent album re-upload can cause a few redundant re-embeds, which are harmless.
- `GET|POST /admin/api/automatic/embedding` reads or flips the automatic local embedding switch. The POST body is `{"enabled":true}` or `{"enabled":false}`; the answer is `{"enabled":...,"running":...}`, where `running` means the worker loop exists (model loaded and started), not that inference is active this instant. Automatic embedding is enabled on every server start; the pause preference is process-local, not persisted, and takes effect between batches — an in-flight batch finishes, and no new batch is claimed until the switch flips back. Invalid control JSON (missing/null `enabled` or unknown fields) returns 400 `INVALID_REQUEST`; a service that cannot answer returns 503 `UNAVAILABLE`.
- `POST /admin/api/encoding/run-local` starts one background local WebP encoding drain with the strict body `{}`, answering `{"started":bool}` — `false` means a drain is already in flight. The target set is the snapshot taken at trigger time, so blobs uploaded afterwards wait for the next trigger. Host deployments need `cwebp` on the server's `PATH` (Debian: `apt-get install webp`; the Docker runtime includes it); a missing `cwebp` fails the trigger immediately with a clear 503 error instead of failing every target. External workers and their leases are unaffected by these local controls.

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
