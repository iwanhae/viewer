# Viewer — Design Notes

`README.md` walks through the request flow and the deployment settings. This note
keeps the parts that are easy to get wrong later: the exact S3 key layout, the
SQLite schema, and the reasoning behind the choices. Every statement below is
taken from the code in `internal/pipeline`, `internal/catalog`, `internal/images`
and `internal/recommend`.

## S3 key layout

The bucket holds binary payloads only; every piece of metadata is in SQLite.

- `batch/<name>.zip` — inbound drop prefix. Only top-level `.zip` objects are
  considered, and the prefix is scanned once at startup.
- `uploads/<albumId>/source.zip` — the staged upload. `albumId` is a UUID for
  `POST /api/albums`; for batch ingest it is the first 16 hex characters of
  `sha256("<etag>:<size>")`, so dropping identical bytes again resolves to the
  same album and the staged object is reused.
- `blobs/<sha256>` — the durable image payload, keyed by the SHA-256 of the raw
  image bytes.

Every key above is logical. `S3_PREFIX` prepends one deployment-owned prefix to
all of them (`photos/blobs/<sha256>` and so on); it is added and removed entirely
inside `internal/storage`, so callers and the catalog only ever see the logical
form. `ListBatchObjects` is the single method that reads keys back out of the
bucket, so it strips the prefix again — the batch scanner feeds listed keys
straight into `CopyObject`/`DeleteObject`, which would otherwise double it.

There is no per-album manifest object. The album-to-photo mapping exists only in
SQLite. Feed, viewer and search requests are answered from the catalog; S3 is
read by key (image bytes) or by the `batch/` prefix.

## SQLite schema

`$STATE_DIR/viewer.db` (default `/tmp/viewer-cache`), opened with WAL,
`foreign_keys(1)`, `synchronous(NORMAL)`, a single connection and a 10 second
busy timeout. The decoded-image cache and the zip staging directory are siblings
under the same `STATE_DIR`.

```sql
albums(id, original_filename, size_bytes, status, source_key,
       photo_count, error, created_at, updated_at)

blobs(hash, size_bytes, content_type, embedding_status, embedding,
      embedding_error, created_at)

photos(album_id, idx, name, hash, width, height, ratio,
       PRIMARY KEY (album_id, idx))
```

Indexed on `albums(status)`, `blobs(embedding_status)` and `photos(hash)`.
`albums.id` and `blobs.hash` are the primary keys, and `photos` is keyed by
`(album_id, idx)`.

Album status is persisted as `QUEUED` / `PROCESSING` / `SUCCEEDED` / `FAILED`
(`albums.error` carries the failure text). Blob embedding status is `pending` /
`ready` / `failed`. Opening the catalog rewrites the pre-merge `PENDING` and
`READY` album rows, so an existing database keeps working.

## Decisions and why

- **Blobs are content-addressed by the raw bytes.** `blobs/<sha256>` means
  identical images share one S3 object, one `blobs` row and one embedding no
  matter how many albums contain them. Extraction heads the key before
  uploading, so those bytes are uploaded once. Dedupe is exact — identical
  bytes, never perceptual similarity.
- **The staged zip is deleted after a successful extraction.** The zip is a
  transport container, not durable state: once every image is in `blobs/`,
  keeping it would store the same bytes twice. The delete happens only after the
  album is marked `SUCCEEDED`; on failure the zip is kept and
  `POST /api/albums/{albumId}/finalize` can retry it.
- **Photo indexes come from a case-insensitive filename sort.** `photos.idx` is
  what the API and UI address, so the ordering has to be deterministic. The
  pipeline uses the same sort as the earlier indexer, which keeps existing photo
  indexes stable across upgrades.
- **One worker, one zip, one entry at a time.** A single background goroutine
  processes the queue and reads one entry into memory before moving on, so the
  working set is bounded by the largest entry rather than by the archive or the
  queue depth.
- **Photos carry width, height and ratio directly.** They are denormalized so
  album reads never need to join `blobs`.
- **Embeddings live on the blob row.** They are little-endian `float32` vectors
  (`catalog.EncodeVector`), computed once per distinct image. Recommendations
  rank them by cosine similarity and return only cross-album hits, at most one
  photo per album.
- **Images are served from a local disk cache.** `images.Service` materialises a
  blob into `$STATE_DIR/images` with an atomic temp-file rename and reuses the
  cached file, so a repeated wall or viewer load never touches S3.
