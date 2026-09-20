# syntax=docker/dockerfile:1.7
ARG SIGLIP2_MODEL_ID=google/siglip2-base-patch16-224

FROM node:22-bookworm-slim AS frontend-build
WORKDIR /src

COPY frontend/package.json frontend/package-lock.json ./frontend/
RUN npm --prefix frontend ci

COPY frontend ./frontend
RUN mkdir -p /src/internal/web && npm --prefix frontend run build

# GoMLX needs go >= 1.27 and, at build time, network access to resolve modules.
FROM golang:1.27-bookworm AS backend-build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY --from=frontend-build /src/internal/web/static ./internal/web/static

RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/viewer ./cmd/viewer

# The vision tower runs in-process, so the viewer image carries the checkpoint
# instead of talking to a separate recommender service.
FROM python:3.12-bookworm AS model-prefetch
ARG SIGLIP2_MODEL_ID
ENV SIGLIP2_MODEL_ID=${SIGLIP2_MODEL_ID}

RUN pip install --no-cache-dir huggingface_hub==0.29.2
RUN python - <<'PY'
import os
from huggingface_hub import hf_hub_download

target = "/opt/siglip2"
model_id = os.environ["SIGLIP2_MODEL_ID"]
os.makedirs(target, exist_ok=True)
for filename in ("config.json", "model.safetensors"):
    downloaded = hf_hub_download(repo_id=model_id, filename=filename)
    os.replace(downloaded, os.path.join(target, filename))
PY

# Base runtime: the binary plus everything except the checkpoint.
FROM debian:bookworm-slim AS runtime-base

COPY --from=backend-build --chown=65532:65532 /out/viewer /app/viewer

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /app/siglip2 /tmp/viewer-cache/images /tmp/viewer-cache/zips && \
    chown -R 65532:65532 /app /tmp/viewer-cache

USER 65532:65532

ENV PORT=8080 \
    CACHE_DIR=/tmp/viewer-cache/images \
    ZIP_CACHE_DIR=/tmp/viewer-cache/zips \
    DB_PATH=/tmp/viewer-cache/viewer.db \
    INGEST_DELETE_SOURCE=true \
    EMBEDDING_BACKEND=go \
    EMBEDDING_MODEL_ID=/app/siglip2

EXPOSE 8080
ENTRYPOINT ["/app/viewer"]

# Slim image for deployments that mount the checkpoint at /app/siglip2 or set
# EMBEDDING_ENABLED=false. A missing model only degrades, because
# EMBEDDING_REQUIRED defaults to false.
FROM runtime-base AS runtime-slim

ENV EMBEDDING_ENABLED=true

# Default image: self-contained, with the SigLIP2 checkpoint baked in.
FROM runtime-base AS runtime

COPY --from=model-prefetch --chown=65532:65532 /opt/siglip2 /app/siglip2

ENV EMBEDDING_ENABLED=true \
    EMBEDDING_REQUIRED=true
