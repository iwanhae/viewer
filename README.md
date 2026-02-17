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
- Recommendation warmup + background embedding are always enabled.
- `RECO_TOPK_DEFAULT` default recommendation count (default `12`).
- `RECO_TOPK_MAX` max recommendation count (default `48`).
- `RECO_WORKER_CONCURRENCY` number of background embedding workers (default `2`).
- `RECO_WORKER_CMD` command used to run embedding worker (default `RECO_WORKER_MODE=worker ./bin/reco-worker`).
- `RECO_WORKER_REQUEST_TIMEOUT_SECONDS` timeout per embed request (default `120`).
- `RECO_WORKER_RESTART_LIMIT` max worker restarts per hour (default `10`).
- `SIGLIP2_MODEL_ID` model identifier for worker backends (default `google/siglip2-base-patch16-224`).
- `SIGLIP2_DEVICE` embedding device hint (default `cpu`, CPU-only worker build).
- Rust worker downloads SigLIP2 model files from Hugging Face on demand and caches them locally.

Recommendation vectors are persisted in each album's `albums/<album-id>/index.json` under an `embeddings` section.

Rust worker supports two modes:
- Worker mode (Go uses this): `RECO_WORKER_MODE=worker ./bin/reco-worker`
- One-shot debug mode (default): `cat test.jpg | ./bin/reco-worker`

## Commands
- `make build` builds `bin/viewer` and `bin/reco-worker`.
- `make test` runs Playwright e2e and saves screenshots to `samples/`.
- `make test` binds the app to `TEST_PORT` (default `18080`) and sets `E2E_BASE_URL` automatically.

## Observability
- The server logs to stdout/stderr via Go's standard logger.
- Startup warmup now runs in background and streams each loaded album index once into both album cache and recommendation state.
- Request-scoped 500 errors now include request context including:
  - request method/path
  - request ID (Chi request ID middleware)
  - remote IP
  - raw query
  - detailed internal error message
- Panics are logged with stack traces before the 500 response is returned.
