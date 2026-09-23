# viewer

Self-hosted photo album server. Upload a zip of photos; the server stores each image as a content-addressed blob in S3-compatible object storage, catalogs albums in SQLite, computes SigLIP2 image embeddings into an optional external Qdrant server, and serves a React frontend plus a JSON API from a single Go binary. Without Qdrant everything works except the "similar photos" recommendations, which answer with a 503.

## Features

- Zip batch upload via presigned PUT (1 GiB cap).
- Deduplicated content-addressed storage: objects live at `blobs/<sha256>`, so identical images are stored once.
- On-the-fly resized JPEGs (320/640/1024 widths), streamed from S3 and cached with blob-hash ETags.
- Semantic "similar photos" recommendations across albums (SigLIP2-base, 768-dim vectors in Qdrant).
- Lease-based HTTP API so external GPU workers can drain the embedding backlog.
- Admin dashboard with embedding stats and a re-embed recovery trigger.
- SQLite catalog backed up to the bucket and auto-restored on startup.
- Upload drop zone: zips copied straight into the `uploads/` prefix (e.g. with `mc` or `aws s3 cp`) are picked up without a restart.

## Architecture

Object keys live under three prefixes: `uploads/` (staged zips), `blobs/` (content-addressed images), and `backups/` (catalog snapshots). Everything else is SQLite.

1. `POST /api/albums` registers a QUEUED album and returns a presigned PUT for `uploads/<albumId>.zip`; the browser uploads the zip straight to object storage.
2. `POST /api/albums/<id>/finalize` queues extraction.
3. A single-slot worker downloads the zip and stores every image as `blobs/<sha256>`, recording entry name, hash, width, and height in SQLite.
4. Embedding workers — the built-in GoMLX one in-process, plus optional external ones — embed pending blobs and write vectors to Qdrant; the catalog keeps only embedding status.
5. Image requests resolve `(albumId, index)` to a blob hash and stream `blobs/<hash>` from S3, served with `ETag` and `Cache-Control: public, max-age=86400, immutable`.

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
| `STATE_DIR` | no | `/var/lib/viewer` | Absolute directory holding the SQLite catalog (`viewer.db`) and the backup stamp. Must be an absolute path. Mount a volume here — the album-to-photo mapping cannot be rebuilt from the blobs. |
| `PORT` | no | `8080` | HTTP listen port. |
| `QDRANT_URL` | no | (empty) | Base URL of the Qdrant server's REST API; must be an http/https URL with a host. Setting it turns photo recommendations on; leaving it empty runs the viewer without a vector store. |
| `QDRANT_API_KEY` | no | (empty) | Sent as the `api-key` header on every Qdrant request. Sent only when set, so an unauthenticated server needs no placeholder. |
| `QDRANT_COLLECTION` | when `QDRANT_URL` is set | — | Qdrant collection holding the per-photo embedding points. Deliberately no default: the viewer refuses to start with a URL but no collection. |
| `SIGLIP2_MODEL_URL` | no | built-in mirror | Base URL the SigLIP2 checkpoint (`config.json`, `model.safetensors`, `tokenizer.json`) is fetched from on first start into `/app/siglip2`. Mount a prepared directory at `/app/siglip2` to skip the download. |
| `EMBEDDING_WORKER_TOKEN` | no | (empty) | Bearer token required on the external embedding-worker API. Empty disables that check (trusted networks only); the rest of the API is unaffected. |
| `ADMIN_TOKEN` | no | (empty) | Basic-auth password for `/admin`. Empty keeps the admin UI disabled. |
| `ALLOW_BACKUP_OVERWRITE` | no | `false` | Lets the catalog finalizer overwrite the bucket's `backups/viewer.db` with a local database it would otherwise refuse to write (untraceable stamp, or drastically smaller than the backup it would replace). |

Notes:

- Set-but-blank values of `QDRANT_URL`, `EMBEDDING_WORKER_TOKEN`, `QDRANT_API_KEY`, `QDRANT_COLLECTION`, and `ADMIN_TOKEN` are rejected at startup rather than silently meaning "off".
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
| GET | `/api/image/{hash}` | Blob content; `?w=320\|640\|1024` returns a resized JPEG keyed by hash + width in the ETag. |
| GET | `/api/recommendations/{albumId}/{index}` | Cross-album similar photos. |
| GET | `/api/photos/search?q=&limit=` | Nearest photos for a natural-language description; requires `QDRANT_URL`. |
| POST | `/api/embedding/claim`, `/renew`, `/results` | External embedding-worker lease API (bearer-token when `EMBEDDING_WORKER_TOKEN` is set). |
| GET | `/admin` | Dashboard (stats + re-embed trigger); enabled only when `ADMIN_TOKEN` is set. |

## External embedding workers

The built-in embedder is a single in-process goroutine. The same claim/renew/results lease API is open to external workers, so a fleet of GPU boxes can drain a large backlog: claims lease pending blobs, results write vectors to Qdrant and then status to SQLite, and expired leases keep blobs from being stranded. See `recommender/README.md` for a ready-made Python worker (`uv sync`, `.env`, `uv run recommender`).

## Development

- `make build` — builds the frontend if stale (`FORCE=1` to force), then `bin/viewer`.
- `make test` — Go unit and integration tests; no credentials or env file needed.
- `make typecheck` — frontend `tsc --noEmit`.
- `make run` — runs `bin/viewer` with `./.env`.
- `make clean`.

The Go SigLIP2 port (`internal/vision`) is regression-tested against a PyTorch reference in `scripts/vision-reference/`; that test is skipped unless `VISION_REFERENCE_DIR` points at a generated bundle.

## Project structure

```
cmd/viewer/                entry point
internal/                  API, catalog, pipelines, vision, storage — see package docs for detail
frontend/                  React + TypeScript + Vite SPA, built into internal/web/static and embedded
recommender/               standalone Python embedding worker
scripts/vision-reference/  PyTorch reference model for the vision-port test
```
