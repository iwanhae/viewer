# Viewer — Design Notes

`README.md` walks through the request flow and the deployment settings. This note
keeps the parts that are easy to get wrong later: the exact S3 key layout, the
SQLite schema, and the reasoning behind the choices. Every statement below is
taken from the code in `internal/ingest`, `internal/pipeline`, `internal/backup`,
`internal/catalog`, `internal/images` and `internal/recommend`.

## S3 key layout

The bucket holds binary payloads only; every piece of metadata is in SQLite.

- `uploads/<albumId>.zip` — the staged upload and the only prefix the viewer
  reads. `albumId` is a UUID for `POST /api/albums`; for a zip adopted by the
  upload scan it is the first 16 hex characters of `sha256("<etag>:<size>")`, so
  dropping identical bytes again resolves to the same album. A zip may sit at any
  depth under the prefix and keeps the name it was uploaded with.
- `blobs/<sha256>` — the durable image payload, keyed by the SHA-256 of the raw
  image bytes.
- `backups/viewer.db` — the catalog snapshot the finalizer uploads once the
  extraction queue drains. One object, overwritten every time: the timestamp to
  compare against is its Last-Modified, and restoring history is the bucket
  operator's job (S3 versioning), not the viewer's.

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
connection and a 10 second busy timeout. The catalog is the only durable thing
under `STATE_DIR`, next to `viewer.db.backup-stamp` — the marker recording which
bucket backup the local file already reflects — and the transient snapshot and
restore temp files, which only exist for the moment a backup or restore runs.
The only data written anywhere else is the staged zip in the OS temp directory,
and the bucket still holds that.

```sql
albums(id, original_filename, size_bytes, status, source_key,
       photo_count, error, created_at, updated_at)

blobs(hash, size_bytes, content_type, embedding_status, embedding,
      embedding_error, embedding_lease_until, created_at)

photos(album_id, idx, name, hash, width, height, ratio,
       PRIMARY KEY (album_id, idx))

blob_embeddings — vec0 virtual table: hash text primary key,
                  embedding float[768] distance_metric=cosine
```

Indexed on `albums(status)`, `albums(source_key)`, `blobs(embedding_status)` and
`photos(hash)`. `albums.id` and `blobs.hash` are the primary keys, and `photos`
is keyed by `(album_id, idx)`.

Album status is persisted as `QUEUED` / `PROCESSING` / `SUCCEEDED` / `FAILED`
(`albums.error` carries the failure text). Blob embedding status is `pending` /
`ready` / `failed`.

The schema evolves through a forward-only migration chain keyed by
`PRAGMA user_version` (`internal/catalog/migrate.go`): every open applies the
migrations newer than the file's version, each inside one transaction that also
bumps the version — a failure aborts startup with the file untouched. Migration
0001 is the baseline schema plus the data fixes that used to run on every open
(the pre-merge `READY`/`PENDING` album rows are rewritten once by it, not
repeatedly), and 0002 creates `blob_embeddings` and backfills it. A file whose
version is newer than the binary's chain is left alone with a warning, so
restoring a newer backup under an older binary does not brick the store.

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
- **Staged zips are deleted in a batch, after a backup covers them.** The zip
  is a transport container, not durable state, but it is also the only thing
  that can rebuild an album — so nothing deletes one before the bucket holds a
  catalog snapshot that already records its album as finished. The finalizer
  (`internal/backup`) runs when the extraction queue drains, when the
  embedding queue drains, once at boot, and hourly on a timer: the extra
  triggers bound how long catalog writes that no drain covers — embedding
  results landing after their albums, external worker results — can go
  without a backup, and the finalizer skips the upload entirely when the
  catalog has not changed since the last one. Every run is the same
  sequence: snapshot via `VACUUM INTO`, upload to `backups/viewer.db`, record
  the stamp, then batch-delete the `source_key`s of every `SUCCEEDED` and
  `FAILED` album — read from the uploaded snapshot file itself, not the live
  database, so no concurrent writer can slip a status change between the
  backup and the delete. A crash anywhere leaves a leftover zip for the next
  startup to clean, never a lost one. A `FAILED` album's zip is deleted with
  the rest: its extraction error is in the catalog, and `finalize` on it
  would only fail the same way again.
- **The catalog restores from the bucket when the bucket is ahead.** On
  startup the store comes up before the catalog, and `backup.Restore` compares
  the backup object's Last-Modified with `viewer.db.backup-stamp` — the time
  of the last backup this local file is known to reflect, written after every
  successful upload and restore. The stamp, not the database file's mtime,
  because in WAL mode commits can land in the sidecar without touching the
  main file. A database without a stamp — an existing deployment upgrading
  into this, or a hand-replaced catalog — is authoritative and never rolled
  back; a missing database restores outright, which is what makes the state
  volume disposable: wipe it, restart, and the bucket rebuilds the catalog.
  Restoring an older snapshot resurrects albums whose zips have not been
  deleted yet, and the scan re-registers them from those zips — a recovery
  property, not a bug. The same asymmetry is guarded on the write side: the
  finalizer refuses to replace an existing backup with a snapshot that has no
  readable stamp or that is drastically smaller than the backup — the
  footprint of a boot whose restore silently did not happen — unless
  `ALLOW_BACKUP_OVERWRITE=1` says otherwise. A stampless database may be
  authoritative for serving, but it must not destroy the bucket's copy;
  restoring downloads are checked against the SQLite magic header for the
  same reason, and a missing bucket surfaces as an error rather than
  masquerading as an empty namespace.
- **One deployment owns one `S3_PREFIX`.** Two replicas sharing a prefix would
  fight over the same zips and overwrite each other's `backups/viewer.db`;
  that was already true for blobs and staging, and the backup makes it
  stateful.
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
- **The catalog volume holds only the catalog.** `STATE_DIR` is the volume an
  operator mounts, and the only thing on it is `viewer.db`. There is no
  server-side image cache — blobs stream from S3 per request — and the staged
  zip being unpacked lives in the OS temp directory, which startup clears of
  crash leftovers. Putting gigabytes of decoded images on a volume whose purpose
  is to preserve a few megabytes of SQLite would grow the backup with data the
  bucket already holds.
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
- **Embeddings live on the blob row; the vec0 table is only an index.**
  `blobs.embedding` is the little-endian `float32` vector
  (`catalog.EncodeVector`), computed once per distinct image and the source of
  truth. `blob_embeddings` — a sqlite-vec virtual table (bundled CGo-free in
  `modernc.org/sqlite/vec`) keyed by the blob hash with cosine distance — is
  derived state, written inside the same transaction as the blob row by the
  single production write path (`ApplyEmbeddingResults`), so a committed ready
  row is always queryable and a failed row never enters the index. Backfills
  can rebuild it from the blob rows; migration 0002 did exactly that, skipping
  embeddings whose byte length does not match 768 floats. Nothing holds
  embeddings in memory any more: recommendations run `FindNeighborEmbeddings`
  (an exhaustive vec0 KNN — sqlite-vec has no ANN yet — clamped at k=4096,
  ties broken by hash in Go because vec0 permits only `ORDER BY distance`) and
  fan out to photo refs only, which makes the catalog the sole thing to load,
  back up and restore.
- **Images are streamed from S3 per request.** `images.Service` reads the blob
  into memory and serves it through `http.ServeContent`, which answers ranges
  and conditional requests. The browser holds the blob-hash `ETag` under
  `Cache-Control: immutable`, so a repeated wall or viewer load is a `304` that
  never reaches the server; a server-side disk cache would only duplicate what
  the browser and the bucket already hold.
