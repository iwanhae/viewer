# viewer

Photo viewer MVP with:
- Go API server
- React frontend embedded into one Go binary
- Playwright e2e test flow

## Required env
Copy one of:
- `.env.example` for local run
- `.env.test.example` for `make test`

At minimum configure S3 values:
- `S3_ENDPOINT`
- `S3_BUCKET`
- `S3_ACCESS_KEY`
- `S3_SECRET_KEY`

Optional tuning:
- `RANGE_CHUNK_SIZE_BYTES` controls S3 byte-range cache chunk size for ZIP reads (default `131072`).
- Recommendation feature:
- `RECO_ENABLED` enables the background recommendation system (default `true`).
- `RECO_DB_PATH` local metadata/vector state path (default `.cache/reco/reco.db`).
- `RECO_SYNC_INTERVAL_SECONDS` periodic S3 metadata sync interval (default `600`).
- `RECO_TOPK_DEFAULT` default recommendation count (default `12`).
- `RECO_TOPK_MAX` max recommendation count (default `48`).
- `RECO_WORKER_CONCURRENCY` number of background embedding workers (default `2`).
- `RECO_MAX_RETRIES` max retry attempts per embedding job (default `5`).
- `RECO_WORKER_CMD` command used to run embedding worker (default `python3 scripts/reco_worker.py`).
- `SIGLIP2_MODEL_ID` model identifier for worker backends (default `google/siglip2-base-patch16-224`).
- `SIGLIP2_DEVICE` embedding device hint such as `cpu` or `cuda:0` (default `cpu`).
- `VECTOR_BACKEND` vector backend selector (default `embedded`).

## Commands
- `make build` builds frontend and backend into `bin/viewer`.
- `make test` runs Playwright e2e and saves screenshots to `samples/`.
- `make test` binds the app to `TEST_PORT` (default `18080`) and sets `E2E_BASE_URL` automatically.
- `scripts/chunk_bench.sh` runs a chunk-size benchmark and writes CSV outputs to `.cache/bench/...`.

## Observability
- The server logs to stdout/stderr via Go's standard logger.
- Request-scoped 500 errors now include request context including:
  - request method/path
  - request ID (Chi request ID middleware)
  - remote IP
  - raw query
  - detailed internal error message
- Panics are logged with stack traces before the 500 response is returned.
