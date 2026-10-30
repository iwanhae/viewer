# viewer

Self-hosted photo album server. Upload a zip of photos; the server stores each image under its original SHA-256 in S3-compatible object storage, catalogs albums in SQLite, computes SigLIP2 image embeddings into an optional external Qdrant server, and serves a React frontend plus a JSON API from a single Go binary. Without Qdrant everything works except the "similar photos" recommendations, which answer with a 503.

## Features

- Zip batch upload via presigned PUT (1 GiB cap).
- Deduplicated storage: objects live at `blobs/<original-sha256>`, so identical uploaded images are stored once even after optional WebP encoding.
- On-the-fly resized JPEGs (320/640/1024 widths), streamed from S3 and cached with response-byte ETags.
- Admin-controlled local and optional external WebP encoding using `cwebp` Q85, preserving supported EXIF/ICC/XMP metadata and replacing only when smaller.
- Semantic "similar photos" recommendations across albums (SigLIP2-base, 768-dim vectors in Qdrant).
- Lease-based HTTP API so external GPU workers can drain the embedding backlog.
- Protected admin browser for catalog, photo, blob, and album pipeline inspection.
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
| GET | `/admin/api/albums/{id}` | Album processing state and exact scoped counts for its referenced blobs/photos. |
| GET | `/admin/api/extraction-failures` | Cursor-paged failed entries from each album's latest run when terminal, filtered by `reasonCode`. |
| GET / POST | `/admin/api/automatic/embedding` | Read / change automatic local embedding; POST body `{"enabled":true}` or `{"enabled":false}`. |
| POST | `/admin/api/encoding/run-local` | Start one background local WebP encoding drain over the pending non-WebP blobs; body `{}`. |

Automatic local embedding is **enabled by default on every server start**; its
pause preference is process-local, not persisted. Both automatic-control methods
return `{"enabled":boolean,"running":boolean}` with
HTTP 200. `running` means the local worker loop exists (model loaded and
started), not that inference is active this instant. Pausing takes effect
between batches: an in-flight batch finishes, and no new batch is claimed until
the switch is flipped back — there are no manual runs left to hand off to, so
nothing races the pause. Before successful model load the worker does not
exist (`running:false`); the automatic preference can already be changed.
Invalid control JSON (including missing/null `enabled` or
unknown fields) returns 400 `INVALID_REQUEST`; a service that cannot answer
returns 503 `UNAVAILABLE`. All mutations use the existing Admin auth,
same-origin and `X-Admin-Action: 1` guards. External workers and their leases are
unaffected by these local controls.

`POST /admin/api/encoding/run-local` starts one background drain that encodes
every pending non-WebP blob locally with the server's `cwebp`, answering
`{"started":boolean}` — `started:false` means a drain is already in flight.
The target set is the snapshot taken at trigger time, so blobs uploaded
afterwards wait for the next trigger.

Host deployments need `cwebp` on the server's `PATH` for local encoding (Debian:
`apt-get install webp`). The Docker runtime includes this package. A missing
`cwebp` fails the run-local trigger immediately with a clear error instead of
failing every target. No worker token is
needed locally; external encoding routes are absent without one. Encoding spool
recovery always starts on boot, even when external worker routes are disabled.

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
`encodingRaw`, `embeddingRaw`, `encodingErrorCode`, `embeddingErrorCode`,
`encodingLeaseExpired`, `embeddingLeaseExpired`, `encodingPhase`, `skippedReason`,
`limit`, and `cursor`. `q` is a
ASCII-case-insensitive literal substring of the album filename or photo name (`%`,
`_`, and `\` are ordinary characters). `hash` is exact. `encoding` accepts the
overview categories `queued`, `processing`, `validating`, `committing`,
`completed`, `skipped`, `failed`, and `unknown`; `embedding` accepts `queued`,
`processing`, `completed`, `failed`, and `unknown`. The `*Raw`
filters match exact stored states, including states this version does not
recognize. `skippedReason` is `webp` or `nonWebp`, based only on stored content
type. `q` is limited to 256 bytes and either raw-state filter to 128 bytes.
Multiple filters combine with AND. Photo rows include the current raw and
normalized states and error text. Pending embeddings are always `queued`,
regardless of encoding status; encoding does not block embedding claims.
If `missingBlob` is true, the photo row's hash has no blob row: `sizeBytes` and
`contentType` are `null`, and pipeline state/error fields are empty rather than
being classified as an unknown blob.

`GET /admin/api/blobs` accepts `albumId`, exact `hash`, the same encoding,
embedding, raw-state, error-code, lease-expiration, phase, and skipped-reason filters, plus `unreferenced=true|false`,
`limit`, and `cursor`. Blob details include `photoCount` and distinct
`albumCount`, not embedded reference arrays. Use
`GET /admin/api/photos?hash={hash}` to page through photos for a shared blob.
Blob and photo detail responses include nullable `encodingObservation` and
`embeddingObservation` objects with `startedAt`, `finishedAt`,
`statusChangedAt`, `attempts`, `phase`, `errorCode`, and `retryability`.
Observations and event history begin when migration 0006 is applied: existing
rows are backfilled with their current status, zero observed claims, and null
timestamps. No legacy timing or attempt counts are inferred. Existing failures
that cannot be safely classified use `errorCode: "legacy_unknown"`; retryability
is `unknown` unless safe typed evidence is available. An encoding phase of
`reconciling` appears only after the encoding service explicitly marks an
ambiguous commit for reconciliation; the worker lease and commit protocol are
unchanged. The per-blob, per-stage claim sequence is the internal attempt
correlation ID; no new worker token or lease generation is introduced.

The history endpoint accepts optional `stage=encoding|embedding`, `limit` (1–100,
default 50), and a numeric monotonic event-ID `cursor`. Pages are ordered by
increasing event ID; pass `nextCursor` to fetch subsequent events. History reads
show events from the last 90 days. Physical deletion is opportunistic: bounded
batches of up to 500 expired rows are attempted periodically during event
writes, so idle catalogs can retain older rows on disk beyond 90 days. History
only stores structured event/status/error codes: worker tokens, stage keys,
signed URLs, and raw error text are not copied into it. The detail UI renders
null timestamps as unavailable and keeps history paging bounded.
`GET /admin/api/albums` accepts a literal-substring `q`, `status` (`QUEUED`,
`PROCESSING`, `SUCCEEDED`, or `FAILED`), `limit`, and `cursor`. The overview's
library `albums`, `photos`, and `blobs` are inventory totals. Its encoding and
embedding categories, skipped-content-type reasons, and raw-state maps each
count every unique blob exactly once in their respective partition; each item
is `{blobCount}` only. The grouped overview query reads `blobs` without joining
photos, so shared references, dangling photo rows, and unreferenced blobs do not
change pipeline counts. Overview links open the corresponding blob filter.
Library photo/album totals are not apportioned across pipeline states.

Album detail retains the exact scoped `PipelineCounts` contract: `blobCount`
counts distinct blobs, `photoCount` counts photo rows that have a matching blob,
and `albumCount` counts distinct referencing albums (within that album scope).
Its referenced photos are available through `GET /admin/api/photos?albumId={id}`.
Album detail also retains its scoped `missingBlobPhotos` integrity count. The
overview omits the expensive unreferenced-blob and missing-blob-photo checks;
those explicit integrity counts are available on demand in
`GET /admin/api/diagnostics` under `integrity`. `unreferencedBlobs` links to the
exact blob-list filter; there is not currently a list filter for dangling
photo rows.

The admin browser has **이슈** (`/admin/issues`) and **시스템 진단**
(`/admin/diagnostics`) tabs. `GET /admin/api/issues` is a read-only snapshot:
encoding and embedding groups include only blobs whose *current* stage status is
`failed`, and group by the safe observation `errorCode` (missing/legacy codes
are `legacy_unknown`). Each group reports `failureCount` and an explicit
`countUnit: "blobs"`; a blob that failed both stages appears in both groups and
groups are independent. Current `FAILED` album rows form one
`album_extraction / legacy_unknown` group with `countUnit: "albums"`: raw album
errors are not classified or returned by this endpoint. Failed extraction
entries come only from each album's latest run, and only once that run is
**terminal**, grouped by their validated reason code with `countUnit: "entries"`;
a running latest run does not fall back to failures from an older run. Issue
groups contain no photo, album-reference, or related-blob counts.
Groups are ordered by `failureCount` and capped at 100. `truncated` and
`remainingGroups` explicitly report omitted reason groups rather than silently
dropping them. Counts use different units by stage and are not comparable or
additive across stages. The UI links each count to the matching blob, album, or
latest-extraction-entry list.

Pipeline issue links use `encodingErrorCode` and `embeddingErrorCode` on both
`GET /admin/api/photos` and `/admin/api/blobs`. Each filter is exact and only
matches rows whose current corresponding stage is `failed`; missing legacy
observations match `legacy_unknown`. The safe code syntax is lowercase ASCII
letters, digits, and underscores (1–64 characters). The filters combine with
the existing status and other filters using AND. Raw error messages are not
used as issue-group keys. The UI links encoder/embedding groups to these
cursor-paged resource lists, and archive groups to the current `status=FAILED`
album list.

`GET /admin/api/extraction-failures?reasonCode=...&limit=...&cursor=...` is the
matching paged target for an extraction-entry group. It returns entry names,
album identity, run ID, ordinal, and safe reason code; it does not return raw
error detail. Only the album's latest run is included, and only when it is
terminal. Pages are ordered by
`(albumId, ordinal)`, default to 50 rows, and cap at 100; cursors are bound to
the reason and limit. Per-album history remains available via
`GET /admin/api/albums/{id}/extractions?limit=&cursor=` and
`GET /admin/api/albums/{id}/entries?runId=&outcome=&limit=&cursor=`. Without
`runId`, entries select the latest run and report `runSelection: "latest"`; a
running run returns 409 because its entries are not stable. A legacy album
without recorded runs returns 404 rather than implying zero failures. Run
history is append-only, but only its latest run contributes to current issue
groups after it becomes terminal; older runs are not substituted while the
latest run is in progress.

`GET /admin/api/diagnostics` does not mutate queues or start background work.
This endpoint is loaded only when the diagnostics page is opened. Its catalog
measurements include the explicit integrity counts `integrity.unreferencedBlobs`
and `integrity.missingBlobPhotos`, plus stage-specific expired lease counts;
encoding-queued and embedding-queued counts with their oldest available
`status_changed_at` ages; and the encoding `committing` and
`reconciling` counts. Each queue's `unknownAgeCount` counts rows in that exact
reported category without a matching recorded pending observation timestamp.
Queue age never falls back to
`created_at`. The oldest commit start is a recorded observation `started_at`;
`unknownStartCount` keeps rows without a known start visible. These catalog
measurements share one short SQLite read transaction. The optional external
vector count is fetched only after that transaction: its status is `disabled`,
`available`, or `unavailable`, and a vector-store outage leaves catalog
diagnostics available with a sanitized message instead of returning 500. A
disabled store's point count and agreement are null/unmeasured. When available,
`countAgreement` compares external point count with catalog-ready photo pairs;
agreement is a count comparison, not a claim of full vector synchronization or
point-by-point integrity. Model/encoding flags report server feature
configuration separately from activity. `workerActivity` is the embedding API
service's last claim/renew/results attempt stamp since this process started; an
attempt is not a heartbeat and does not prove a worker is alive. Zero means no
attempt has been recorded since restart.

Embedding queue diagnostics and browse filters classify every pending embedding
as `queued`, independently of encoding progress. Claims also reclaim expired
embedding leases without checking encoding status. Legacy encoding gates are
neutralized on upgrade and ignored by claims.

The diagnostics page links expired embedding/encoding leases to exact
`embeddingLeaseExpired=true` / `encodingLeaseExpired=true` filters on photo and
blob lists. `encodingPhase=reconciling` is an exact committing-phase filter.
Queue-age links open the matching category, but do not isolate only the oldest
rows; the UI says so explicitly. These filters are bounded by the same
cursor and 1–100 page limits as other browse APIs.

The browse queries use existing primary-key and hash indexes and add no schema
migration. Literal substring searches use SQLite `instr(lower(...))` and are
not indexed; state-filtered photo/blob pages can also examine many rows on a
large catalog. The current indexes support ordered album/photo/hash traversal,
but production `EXPLAIN QUERY PLAN` and workload measurements should guide any
future approved index migration. No list API reports retry counts or invented
retry/timing estimates.

### Scoped admin recovery (stage 7A backend)

The scoped recovery API currently supports exactly four blob actions:

- `retry_failed_encoding`: only raw `encoding_status=failed` non-WebP blobs.
- `retry_failed_embedding`: only raw `embedding_status=failed` blobs.
- `release_expired_encoding`: only expired `encoding_status=leased` blobs.
- `release_expired_embedding`: only expired `embedding_status=processing` blobs.

Encoding `received` and `committing` work is never reset by these actions;
`reconciling` observations are ineligible. Live leases, ready rows, `skipped`
(including not-smaller encodings), pending rows, and all other states are
reported as skipped. The legacy global `/admin/api/recover` and `/admin/api/reindex`
policies remain available and retain their existing broader/explicitly
dangerous behavior; the scoped endpoints do not add whole-library reindexing.
Album extraction retry is deliberately not offered: the existing extraction
queue is in-memory and does not provide the durable idempotent enqueue needed
to safely persist a retry across a crash. No S3 source check is therefore
performed by this backend.

Create a preview with strict JSON (unknown fields and non-JSON content types are
rejected). A selection contains exactly one of `hashes`, `photos`, `albumIds`,
or `filter`. Explicit photo identities use `(albumId, idx)`; albums/photos
resolve to distinct content hashes. `filter` uses the same exact filters as
`GET /admin/api/blobs` (for example `encoding`, `embedding`, `albumId`, `hash`,
error-code, lease-expired, phase, skipped-reason, and `unreferenced` filters).
There is no `q` substring filter on the blob browse endpoint. Example:

```http
POST /admin/api/actions/preview
Content-Type: application/json
X-Admin-Action: 1

{"action":"retry_failed_embedding","selection":{"albumIds":["album-a"]}}
```

The response includes `previewId`, `actor`, `actorNote`, `action`, creation and
expiry timestamps, `targetCount`, `eligibleCount`, `skippedCount`,
`skippedReasons`, `selectedPhotos`, `affectedPhotos`, `affectedAlbums`, and
`sharedBlobWarning`. Previews expire after ten minutes. The target cap is 10,000
distinct blobs and explicit inputs are capped at 10,000 items; an oversized
filter is rejected, never truncated. The stored target set is immutable: rows
that begin matching after preview are not added. Preview and GET calls do not
mutate pipeline state.

For album/photo scopes, `selectedPhotos` counts photo rows in that explicit
scope while `affectedPhotos`/`affectedAlbums` count every current reference to
the selected shared blobs. `sharedBlobWarning` is true when global photo impact
exceeds the selected scope. For hash and blob-filter scopes, selected and global
photo counts describe the same blob-wide scope. Preview eligibility is only a
snapshot; execution rechecks each target's generation and uses a transactional
compare-and-set, recording a `conflict` rather than overwriting changed state.
The private fingerprint covers raw stage state, observation timestamps/phase,
attempt and safe error metadata, plus an encoding lease/token digest when
releasing an encoding lease. Lease tokens themselves are never returned or
stored in recovery tables.

Queue the preview with a caller-generated idempotency key:

```http
POST /admin/api/operations
Content-Type: application/json
X-Admin-Action: 1
Idempotency-Key: incident-2026-10-14-1

{"previewId":"<previewId>","action":"retry_failed_embedding"}
```

The same key and exact preview/action/payload returns the same operation; a
different binding returns `409`. A preview can be consumed once. Operations
are durable in SQLite and execute in bounded small batches; startup resumes
queued or interrupted work, while shutdown returns unfinished work to the
queue. Operation statuses are `queued`, `running`, `completed`,
`partially_failed`, or `failed`. Per-target results include `succeeded`,
`conflict`, `skipped`, and `failed`, with short safe reason codes only. Use
`GET /admin/api/operations?limit=50&cursor=...` for summaries and
`GET /admin/api/operations/{id}?limit=50&cursor=...` for results; page sizes
are 1–100. Result rows are never returned unbounded.

The operation executor starts only when `ADMIN_TOKEN` is nonempty. While the
admin service is disabled, durable operations stay paused and no targets are
executed; enabling the admin service at startup requeues interrupted operations
and resumes queued work.

The **복구 운영** frontend shows a cursor-paged audit list and cursor-paged
per-blob operation results, with links to blob details. Photo and blob browse
lists allow row checkboxes and “select visible page” only; selection is cleared
when URL filters or page paths change. Photos submit explicit `(albumId, idx)`
identities and the backend deduplicates shared blob hashes. Blob lists also
offer a separate whole-filter preview using only exact supported blob filters;
`q`, `cursor`, duplicate, and unknown query parameters disable that action
rather than being discarded. Filter previews are capped at 10,000 distinct
blobs and oversized requests are rejected, not truncated.

Every scoped action follows preview → review → explicit confirmation → queued
operation. The preview shows eligibility, skipped reasons, expiry, and
photo/album impact including shared-blob impact. Changing the action or
selection invalidates the preview. A network failure while queueing retains
that preview's idempotency key so the exact request can safely be retried.
Queued/running operation details refresh every 20 seconds while visible,
without overlapping requests. Per-target `succeeded` results are labeled as
applied; operation completion means recovery execution ended. Applied targets
were returned to pipeline queues, not that encoding or embedding finished
successfully. The audit actor is only
the HTTP Basic username label and does not verify an individual token holder.
The existing global recovery screen remains separately labeled as a dangerous
legacy action. Album detail recovery scopes blob pipeline actions to that album;
archive extraction retry is unavailable and is not implied by this UI.

The operation actor is the username label from HTTP Basic credentials, not a
verified individual: deployments share an admin token, and the API explicitly
does not claim to identify which token holder acted. Audit rows store this
label, action, preview, idempotency binding, state, and safe result reason codes;
they do not store credentials or raw pipeline errors.

Migration 0008 adds `admin_recovery_previews` (actor/action/scope, timestamps,
counts and one-time consumption), `admin_recovery_targets` (ordered frozen blob
IDs, eligibility, safe reason, private generation fingerprint and impact
counts), `admin_recovery_operations` (idempotency binding and durable status),
and `admin_recovery_results` (one bounded-pageable result per frozen target).
Catalog state transition, stage observation/event, and result are committed in
the same SQLite transaction for each target.

Every mutation under `/admin`, including legacy `/admin/api/recover` and
`/admin/api/reindex`, requires the custom
`X-Admin-Action: 1` header; the legacy dashboard adds it to its existing POSTs.
Legacy `/admin/api/recover` requests and all new POST routes require
`Content-Type: application/json`; the existing bodyless `/admin/api/reindex`
request is the exception and does not need a content type. Requests with an
explicit cross-origin `Origin` or non-same-origin `Sec-Fetch-Site` are rejected.
No CORS permission is configured, so cross-origin preflight is not enabled.
Existing GET routes, including authenticated curl reads, do not require the
custom header.

When TLS terminates at a reverse proxy, the same-origin scheme check uses
`X-Forwarded-Proto` and compares `Origin` against the public `Host`. Only use
this behind a known proxy that overwrites/sanitizes the forwarded header; the
application does not validate trusted proxies, so direct clients must not be
able to spoof it.

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
