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
