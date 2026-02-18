# syntax=docker/dockerfile:1.7
ARG SIGLIP2_MODEL_ID=google/siglip2-base-patch16-224
ARG MODEL_BASE_IMAGE=model-base
ARG RECOMMENDER_ACCEL=none

FROM node:22-bookworm-slim AS frontend-build
WORKDIR /src

COPY frontend/package.json frontend/package-lock.json ./frontend/
RUN npm --prefix frontend ci

COPY frontend ./frontend
RUN mkdir -p /src/internal/web && npm --prefix frontend run build

FROM golang:1.25-bookworm AS backend-build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY --from=frontend-build /src/internal/web/static ./internal/web/static

RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/viewer ./cmd/viewer

FROM rust:1.90-bookworm AS recommender-build
WORKDIR /src
ARG RECOMMENDER_ACCEL
ARG TARGETARCH

COPY workers/recommender/Cargo.toml workers/recommender/Cargo.lock ./workers/recommender/
COPY workers/recommender/src ./workers/recommender/src

RUN set -eux; \
    if [ "${RECOMMENDER_ACCEL}" = "mkl" ] && [ "${TARGETARCH:-}" = "amd64" ]; then \
        echo "building recommender with MKL acceleration for TARGETARCH=${TARGETARCH}"; \
        cargo build --manifest-path workers/recommender/Cargo.toml --release --features mkl; \
    else \
        if [ "${RECOMMENDER_ACCEL}" = "mkl" ]; then \
            echo "RECOMMENDER_ACCEL=mkl requested for TARGETARCH=${TARGETARCH:-unknown}; building without MKL"; \
        else \
            echo "building recommender without MKL acceleration"; \
        fi; \
        cargo build --manifest-path workers/recommender/Cargo.toml --release; \
    fi; \
    mkdir -p /out; \
    cp workers/recommender/target/release/recommender /out/recommender

FROM python:3.12-bookworm AS model-prefetch
ARG SIGLIP2_MODEL_ID
ENV HF_HOME=/opt/hf-home \
    SIGLIP2_MODEL_ID=${SIGLIP2_MODEL_ID}

RUN pip install --no-cache-dir huggingface_hub==0.29.2
RUN python - <<'PY'
import os
from huggingface_hub import hf_hub_download

model_id = os.environ["SIGLIP2_MODEL_ID"]
for filename in ("config.json", "model.safetensors"):
    hf_hub_download(repo_id=model_id, filename=filename)
PY

FROM debian:bookworm-slim AS model-base
ARG SIGLIP2_MODEL_ID
WORKDIR /app

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=model-prefetch --chown=65532:65532 /opt/hf-home /tmp/hf-home

RUN mkdir -p /tmp/viewer-cache/images /tmp/viewer-cache/zips && \
    chown -R 65532:65532 /app /tmp/viewer-cache /tmp/hf-home

FROM ${MODEL_BASE_IMAGE} AS runtime
ARG SIGLIP2_MODEL_ID

COPY --from=backend-build --chown=65532:65532 /out/viewer /app/viewer
COPY --from=recommender-build --chown=65532:65532 /out/recommender /app/recommender
COPY --chown=65532:65532 docker/entrypoint.sh /app/entrypoint.sh

RUN chmod +x /app/entrypoint.sh

USER 65532:65532

ENV PORT=8080 \
    CACHE_DIR=/tmp/viewer-cache/images \
    ZIP_CACHE_DIR=/tmp/viewer-cache/zips \
    RECOMMENDER_REQUIRED=false \
    HF_HOME=/tmp/hf-home \
    SIGLIP2_MODEL_ID="${SIGLIP2_MODEL_ID}" \
    RECOMMENDER_LISTEN_ADDR="0.0.0.0:18081" \
    RECOMMENDER_ENDPOINT="http://127.0.0.1:18081"

EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
