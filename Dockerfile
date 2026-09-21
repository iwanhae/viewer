# syntax=docker/dockerfile:1.7

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

# Runtime: the Go viewer plus frontend assets, without the SigLIP2 checkpoint.
# A cold start fetches the checkpoint into /app/siglip2 from SIGLIP2_MODEL_URL
# (default internal/config.DefaultModelURL), which keeps the published image
# small and the build free of any dependency on the upstream model host. Mount
# a volume - or a prepared directory holding config.json and model.safetensors
# - at /app/siglip2 to keep the download across container replacements. Without
# a checkpoint the viewer logs the load failure at startup and serves
# recommendations from whatever embeddings the catalog already holds.
FROM debian:bookworm-slim

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
