# Viewer — Design Notes

`README.md` walks through the request flow and the deployment settings. This note
keeps the parts that are easy to get wrong later: the exact S3 key layout, the
SQLite schema, and the reasoning behind the choices. Every statement below is
taken from the code in `internal/ingest`, `internal/pipeline`, `internal/catalog`,
`internal/images` and `internal/recommend`.

## S3 key layout

The bucket holds binary payloads only; every piece of metadata is in SQLite.

- `uploads/<albumId>.zip` — the staged upload and the only prefix the viewer
  reads. `albumId` is a UUID for `POST /api/albums`; for a zip adopted by the
  upload scan it is the first 16 hex characters of `sha256("<etag>:<size>")`, so
  dropping identical bytes again resolves to the same album. A zip may sit at any
  depth under the prefix and keeps the name it was uploaded with.
- `blobs/<sha256>` — the durable image payload, keyed by the SHA-256 of the raw
  image bytes.

The staging key is generated once and then stored on the album row
(`albums.source_key`), and the pipeline reads it back from there rather than
recomputing it. The generated name can therefore change between releases without
stranding objects an older release already staged. The scan resolves a listed key
back to an album through that column, and falls back to the key `SourceKey`
generates for rows written before the column was populated.

Every key above is logical. `S3_PREFIX` prepends one deployment-owned prefix to
all of them (`photos/blobs/<sha256>` and so on); it is added and removed entirely
inside `internal/storage`, so callers and the catalog only ever see the logical
form. `ListObjects` is the single method that reads keys back out of the bucket,
so it strips the prefix again — the scan feeds listed keys straight back into the
album lookup and the pipeline, which would otherwise see it doubled.

There is no per-album manifest object and no object is ever copied. The
album-to-photo mapping exists only in SQLite. Feed, viewer and search requests
are answered from the catalog; S3 is read by key (image bytes) or by the
`uploads/` prefix.

## SQLite schema

`$STATE_DIR/viewer.db` (default `/var/lib/viewer`, the path the image declares as
a volume), opened with WAL, `foreign_keys(1)`, `synchronous(NORMAL)`, a single
connection and a 10 second busy timeout. The catalog is the only thing under
`STATE_DIR`. The decoded-image cache and the zip staging directory live under
`config.CacheRoot` (`/tmp/viewer-cache`) instead, because both are rebuilt from
the bucket and must not ride along on the volume that keeps the catalog.

```sql
albums(id, original_filename, size_bytes, status, source_key,
       photo_count, error, created_at, updated_at)

blobs(hash, size_bytes, content_type, embedding_status, embedding,
      embedding_error, created_at)

photos(album_id, idx, name, hash, width, height, ratio,
       PRIMARY KEY (album_id, idx))
```

Indexed on `albums(status)`, `albums(source_key)`, `blobs(embedding_status)` and
`photos(hash)`. `albums.id` and `blobs.hash` are the primary keys, and `photos`
is keyed by `(album_id, idx)`.

Album status is persisted as `QUEUED` / `PROCESSING` / `SUCCEEDED` / `FAILED`
(`albums.error` carries the failure text). Blob embedding status is `pending` /
`ready` / `failed`. Opening the catalog rewrites the pre-merge `PENDING` and
`READY` album rows, so an existing database keeps working.

The two statuses are independent and the API keeps them that way: an album is
`SUCCEEDED` once its zip has been extracted, which says nothing about whether
its blobs have embeddings. Embedding coverage is reported separately, by
`catalog.EmbeddingCounts` (every blob) and `catalog.EmbeddingCountsByAlbum` (the
distinct blobs of one album, so a blob shared by two albums counts for both).
A blob that is `failed` is terminal — `ListBlobsAwaitingEmbedding` selects only
`pending` rows — which is why the progress payload reports `failed` next to
`ready` rather than folding it into a single percentage.

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
- **`uploads/` is the one staging prefix, and the upload scan watches it.** A
  client puts its zip there through a presigned PUT; an operator or an external
  tool can put one there directly. The scan (startup plus one pass a minute)
  adopts what no album owns and re-queues an album whose zip is still waiting,
  which replaced the separate startup-only batch scan and pending-enqueue pass.
  It copies nothing: the two prefixes it used to shuffle objects between existed
  only because the drop zone and the staging area were different places.
- **The scan never writes an album status.** Only the pipeline moves an album
  between `QUEUED`, `PROCESSING` and its terminal state, so a scan that runs
  while an extraction is in flight cannot flip it back to `QUEUED` — which for a
  startup-only pass was harmless but for a repeating one would strand an album
  that had just been marked `SUCCEEDED` with its zip already deleted. A
  duplicate enqueue is ignored by the pipeline, so re-queueing a live album is
  free and a stale `PROCESSING` row heals itself on the next pass.
- **The catalog and the caches are separate directories.** `STATE_DIR` is the
  volume an operator mounts, and the only thing on it is `viewer.db`; the caches
  are a fixed `/tmp` path because a miss is refetched. Putting gigabytes of
  decoded images on a volume whose purpose is to preserve a few megabytes of
  SQLite would grow the backup with data the bucket already holds.
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
  blob into `/tmp/viewer-cache/images` with an atomic temp-file rename and reuses
  the cached file, so a repeated wall or viewer load never touches S3.
