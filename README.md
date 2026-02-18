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
  - `RECO_TOPK_DEFAULT` default recommendation count (default `12`).
  - `RECO_TOPK_MAX` max recommendation count (default `48`).
- Recommender service:
  - `RECOMMENDER_ENDPOINT` endpoint for the Rust recommender service (example `http://127.0.0.1:18081`).
  - `RECOMMENDER_REQUIRED` whether startup must fail when recommender is unavailable (`true` in development and compose; `false` in Docker image for first-run model downloads).
  - `RECOMMENDER_CONCURRENCY` number of background embedding workers (`0` means auto, default is adaptive `3x GOMAXPROCS`, min `8`, max `64`).
  - `RECOMMENDER_REQUEST_TIMEOUT_SECONDS` timeout per embed request (default `120`).
- `SIGLIP2_MODEL_ID` model identifier for worker backends (default `google/siglip2-base-patch16-224`).
- `SIGLIP2_DEVICE` embedding device hint (default `cpu`, CPU-only worker build).
- `HF_HOME` Hugging Face cache root used by the Rust worker. In Docker runtime this is pre-populated at `/tmp/hf-home`.

Rust recommender service endpoints:
- `GET /ping`
- `GET /healthz`
- `POST /embed` with JSON `{"request_id","image_b64","model_id","device"}`

Rust worker resolves model files through the Hugging Face cache (`HF_HOME`), downloading only if a required file is missing.

Recommendation vectors are persisted in each album's `albums/<album-id>/index.json` under an `embeddings` section.
Background embedding now runs album-by-album: workers pick a random album with missing vectors, embed all pending photos in that album, then persist metadata in one write.
Recommendation responses are cross-album only: photos from the same album as the query are excluded from results.
If no cross-album neighbors exist for an embedded query photo, the recommendation status returns `ready` with an empty `items` list.

In the Docker image, the container entrypoint starts both the Rust recommender service and the Go server.
The image prefetches `config.json` and `model.safetensors` for `SIGLIP2_MODEL_ID` at build time, so pod startup does not require Hugging Face egress.
CI publishes a reusable model base image (`model-siglip2-base-patch16-224`) and app images can reuse it with `--build-arg MODEL_BASE_IMAGE=<registry/repo:tag>`.
Local `docker build` keeps working without overrides by defaulting `MODEL_BASE_IMAGE` to the in-file `model-base` stage.
To use a different model in Docker, build with `--build-arg SIGLIP2_MODEL_ID=<repo-id>` and publish a corresponding model base image tag.
To build an MKL-accelerated recommender for x86_64 images, pass `--build-arg RECOMMENDER_ACCEL=mkl --platform linux/amd64`.
`RECOMMENDER_ACCEL` defaults to `none`; if `mkl` is requested on non-`amd64` targets, the Dockerfile falls back to a non-MKL recommender build.

## Commands
- `make build` runs `go mod tidy`, builds `bin/recommender` in release mode, builds frontend assets, and compiles `bin/viewer`.
- `make test` runs `make build`, then `go test ./...`, then Playwright e2e and saves screenshots to `samples/` by default.
- `make test` binds the app to `TEST_PORT` (default `18080`) and sets `E2E_BASE_URL` automatically.
- `make run` starts `bin/viewer` (loads `.env` if present, does not rebuild binaries).
- `make clean` removes build outputs and dependency caches.

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
