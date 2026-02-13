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

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=backend-build /out/viewer /app/viewer

ENV PORT=8080 \
    CACHE_DIR=/tmp/viewer-cache/images \
    ZIP_CACHE_DIR=/tmp/viewer-cache/zips

EXPOSE 8080
ENTRYPOINT ["/app/viewer"]
