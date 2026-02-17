# syntax=docker/dockerfile:1.7

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

FROM rust:1.90-bookworm AS reco-worker-build
WORKDIR /src

COPY workers/reco-worker/Cargo.toml workers/reco-worker/Cargo.lock ./workers/reco-worker/
COPY workers/reco-worker/src ./workers/reco-worker/src

RUN cargo build --manifest-path workers/reco-worker/Cargo.toml --release && \
    mkdir -p /out && \
    cp workers/reco-worker/target/release/reco-worker /out/reco-worker

FROM debian:bookworm-slim AS runtime
WORKDIR /app

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=backend-build /out/viewer /app/viewer
COPY --from=reco-worker-build /out/reco-worker /app/reco-worker

RUN mkdir -p /tmp/viewer-cache/images /tmp/viewer-cache/zips && \
    chown -R 65532:65532 /app /tmp/viewer-cache

USER 65532:65532

ENV PORT=8080 \
    CACHE_DIR=/tmp/viewer-cache/images \
    ZIP_CACHE_DIR=/tmp/viewer-cache/zips \
    RECO_WORKER_CMD="RECO_WORKER_MODE=worker /app/reco-worker"

EXPOSE 8080
ENTRYPOINT ["/app/viewer"]
