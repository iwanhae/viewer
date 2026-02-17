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

FROM rust:1.90-bookworm AS recommender-build
WORKDIR /src

COPY workers/recommender/Cargo.toml workers/recommender/Cargo.lock ./workers/recommender/
COPY workers/recommender/src ./workers/recommender/src

RUN cargo build --manifest-path workers/recommender/Cargo.toml --release && \
    mkdir -p /out && \
    cp workers/recommender/target/release/recommender /out/recommender

FROM debian:bookworm-slim AS runtime
WORKDIR /app

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=backend-build /out/viewer /app/viewer
COPY --from=recommender-build /out/recommender /app/recommender
COPY docker/entrypoint.sh /app/entrypoint.sh

RUN mkdir -p /tmp/viewer-cache/images /tmp/viewer-cache/zips && \
    chmod +x /app/entrypoint.sh && \
    chown -R 65532:65532 /app /tmp/viewer-cache

USER 65532:65532

ENV PORT=8080 \
    CACHE_DIR=/tmp/viewer-cache/images \
    ZIP_CACHE_DIR=/tmp/viewer-cache/zips \
    RECOMMENDER_REQUIRED=false \
    RECOMMENDER_LISTEN_ADDR="0.0.0.0:18081" \
    RECOMMENDER_ENDPOINT="http://127.0.0.1:18081"

EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
