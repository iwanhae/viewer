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
import shutil

from huggingface_hub import hf_hub_download

target = "/opt/siglip2"
model_id = os.environ["SIGLIP2_MODEL_ID"]
os.makedirs(target, exist_ok=True)
for filename in ("config.json", "model.safetensors"):
    # hf_hub_download returns a path inside the Hub cache, and that path is a
    # symlink into the cache's blob directory. Renaming it would move the link
    # and leave a dangling file inside the image, which the loader then reports
    # as "file not found in local model directory". Copy the resolved contents
    # instead, so what lands in the image is a real file.
    downloaded = hf_hub_download(repo_id=model_id, filename=filename)
    destination = os.path.join(target, filename)
    shutil.copyfile(os.path.realpath(downloaded), destination)
    print(f"prefetched {filename}: {os.path.getsize(destination)} bytes")
PY
# Fail the build instead of shipping an incomplete checkpoint: at runtime a
# missing file only degrades to "serving without embeddings", which is easy to
# miss.
RUN test -s /opt/siglip2/config.json && test -s /opt/siglip2/model.safetensors

# Base runtime: the binary plus everything except the checkpoint.
FROM debian:bookworm-slim AS runtime-base

COPY --from=backend-build --chown=65532:65532 /out/viewer /app/viewer

# /var/lib/viewer is the persistent state directory (the SQLite catalog).
# Everything else the process writes - the staged zip being unpacked - goes to
# the world-writable /tmp, which the container owns alone.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /app/siglip2 /var/lib/viewer && \
    chown -R 65532:65532 /app /var/lib/viewer

USER 65532:65532

# The catalog is the only record of which album holds which photos and cannot be
# rebuilt from the bucket, so give it a volume of its own. An operator can point
# STATE_DIR elsewhere and mount there instead.
VOLUME ["/var/lib/viewer"]

EXPOSE 8080
ENTRYPOINT ["/app/viewer"]

# Slim image for deployments that mount the checkpoint at /app/siglip2. Without
# it the viewer logs the load failure at startup and serves recommendations from
# whatever embeddings the catalog already holds.
FROM runtime-base AS runtime-slim

# Default image: self-contained, with the SigLIP2 checkpoint baked in.
FROM runtime-base AS runtime

COPY --from=model-prefetch --chown=65532:65532 /opt/siglip2 /app/siglip2
# A checkpoint that arrived as a broken symlink or an empty file builds a broken
# image, so check the copied tree rather than trusting the prefetch stage.
RUN test -s /app/siglip2/config.json && test -s /app/siglip2/model.safetensors
