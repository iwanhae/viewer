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

FROM python:3.13-slim-bookworm AS runtime
WORKDIR /app

COPY pyproject.toml uv.lock ./
RUN python3 -m pip install --no-cache-dir --upgrade pip && \
    python3 -m pip install --no-cache-dir \
      numpy \
      onnxruntime \
      pillow \
      torch \
      transformers==5.1.0

COPY --from=backend-build /out/viewer /app/viewer
COPY scripts/reco_worker.py /app/scripts/reco_worker.py

RUN mkdir -p /tmp/viewer-cache/images /tmp/viewer-cache/zips && \
    chown -R 65532:65532 /app /tmp/viewer-cache

USER 65532:65532

ENV PORT=8080 \
    CACHE_DIR=/tmp/viewer-cache/images \
    ZIP_CACHE_DIR=/tmp/viewer-cache/zips \
    RECO_WORKER_CMD="RECO_WORKER_MODE=worker python3 scripts/reco_worker.py"

EXPOSE 8080
ENTRYPOINT ["/app/viewer"]
