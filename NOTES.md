# Viewer — Final Design (Go + TypeScript)

## 1. Goals

1. View a large personal photo archive organized as **ZIP albums** (≈10k ZIPs, ~20 photos each).
2. Keep storage efficient on a **home S3-compatible server** that performs poorly with many small objects.
3. Provide a **photo-centric UI** with:
   - **Page 1:** Random “Pinterest-like” wall (edge-to-edge)
   - **Page 2:** Full-screen album viewer (edge-to-edge)
4. Keep metadata in a **local SQLite catalog** and store image bytes in S3 as
   **content-addressed blobs**, so identical images are never stored twice.
   (Superseded the original "no database, one index.json per ZIP" plan.)

## 2. Non-Goals (for MVP)

- Search, tagging, ML classification
- Sharing/auth beyond minimal single-user auth
- Full offline indexing of every photo ahead of time
- Generating/storing per-photo thumbnails as separate S3 objects

------

## 3. High-Level Architecture

### Components

1. **Web Frontend (TypeScript)**
   - Upload ZIP
   - Page 1 random wall
   - Page 2 full-screen viewer
2. **API Server (Go)**
   - Album upload session + finalize
   - Sequentially download each staged zip and extract every image entry
   - Record album/photo/blob metadata in the local SQLite catalog
   - Random feed API
   - Image serving API (read `blobs/<hash>`; local disk cache)
3. **S3-Compatible Storage (Home Server)**
   - Stores the staged upload zip (deleted after a successful extract)
   - Stores one object per distinct image content: `blobs/<sha256>`
4. **Local SQLite Catalog + Disk Cache (API Server)**
   - SQLite is the source of truth for albums, photos and embeddings
   - Disk cache holds recently served image blobs

------

## 4. Storage Layout (S3 Keys)

Bucket: `photo-archive`

Staging (deleted after a successful extract):

- `uploads/{albumId}/source.zip`

Durable image payloads (one object per distinct content hash, shared by every
album that contains the same bytes):

- `blobs/{sha256}`

No per-album metadata objects and no per-photo thumbnail objects in S3.

------

## 5. Metadata (SQLite Catalog)

Purpose:

- Allow the UI and APIs to:
  - list photos in order
  - compute masonry layout using aspect ratio
  - address a photo by `(albumId, photoIndex)`
  - serve image bytes via the photo's content hash

Tables (see `internal/catalog`):

```sql
albums(id, original_filename, size_bytes, status, source_key,
       photo_count, error, created_at, updated_at)

blobs(hash, size_bytes, content_type, embedding_status, embedding,
      embedding_error, embedding_updated_at, created_at, updated_at)

photos(album_id, idx, name, hash, width, height, ratio,
       PRIMARY KEY (album_id, idx))
```

Notes:

- `albums.status` moves through `PENDING -> QUEUED -> PROCESSING -> READY`
  (`FAILED` on error) and drives the `/finalize` status API.
- `photos.name` is the original zip entry name; `photos.idx` is the stable
  ordering index (entries sorted by lower-cased filename).
- `photos.hash` points at a `blobs` row, which is where embeddings are stored.
  Identical image bytes therefore share one blob row, one S3 object and one
  embedding regardless of how many albums contain them.
- `AlbumIndex` is still the JSON shape returned by `GET /api/albums/{albumId}`;
  it is now assembled from `albums` + `photos`.

------

## 6. Core APIs

### 6.1 Create Album Upload Session

```
POST /api/albums
```

Request:

```json
{ "filename": "my_album.zip", "sizeBytes": 123456789 }
```

Response:

```json
{
  "albumId": "uuid-or-hash",
  "upload": {
    "method": "PUT",
    "url": "presigned-url",
    "headers": { }
  }
}
```

Behavior:

- Server decides `albumId`.
- Server returns a presigned PUT URL to upload `albums/{albumId}/source.zip`.

### 6.2 Finalize Upload (Trigger Metadata Generation)

```
POST /api/albums/{albumId}/finalize
```

Response:

```json
{ "status": "INDEXING" }
```

Behavior:

- Server verifies the staged ZIP exists in S3.
- The album is marked `QUEUED` and handed to the in-process pipeline worker.
- The worker downloads the ZIP, stores each image as a `blobs/<sha256>` object,
  records metadata in SQLite and finally marks the album `READY`, then deletes
  the staged ZIP (`INGEST_DELETE_SOURCE=true`).
- Clients poll `GET /api/albums/{albumId}/finalize` until `SUCCEEDED`/`FAILED`.

### 6.3 Get Album Metadata

```
GET /api/albums/{albumId}
```

Response: the album metadata assembled from the SQLite catalog.

### 6.4 List Albums (Lightweight)

```
GET /api/albums
```

Response:

```json
{
  "albums": [
    { "albumId": "...", "photoCount": 20, "originalFilename": "..." }
  ]
}
```

Implementation:

- Query the local SQLite catalog (`albums` where `status = 'READY'`), cached in
  memory for the feed snapshot.

### 6.5 Random Feed (Page 1)

```
GET /api/feed?limit=80&seed=abc&cursor=...
```

Response:

```json
{
  "items": [
    {
      "albumId": "....",
      "i": 7,
      "w": 4032,
      "h": 3024,
      "ratio": 1.3333,
      "src": "/api/image/a1b2c3/7?mode=wall&w=480"
    }
  ],
  "nextCursor": "..."
}
```

Behavior:

- Randomly sample `(albumId, photoIndex)` pairs from existing albums.
- Returns enough metadata for masonry and a URL to request the image bytes.
- Server can cache album indices in memory for speed.

### 6.6 Serve Image Bytes (On-demand)

`GET /api/image/{albumId}/{i}?mode=wall&w=480`
`GET /api/image/{albumId}/{i}?mode=viewer&max=0`

Modes:

- `wall`:
  - Return a resized image suitable for the wall (e.g., width=480).
- `viewer`:
  - Return the original (or optionally a large “fit” size).

Caching (optional but recommended):

- Disk cache key:
  - `cache/{albumId}/{i}/wall_{w}.jpg`
  - `cache/{albumId}/{i}/viewer_orig.jpg` (or `viewer_{max}.jpg`)

------

## 7. ZIP Indexing Strategy

### 7.1 Minimal Indexing (MVP)

When `/finalize` is called:

1. Download the ZIP to local temp storage (one album at a time, sequentially).
2. Enumerate entries; keep only `.jpg/.jpeg/.png/.webp`.
3. Sort by lower-cased filename to assign stable photo indexes.
4. For each entry:
   - compute the SHA-256 of the raw bytes,
   - extract width/height from the image header,
   - upload the bytes to `blobs/<sha256>` (skipped when the object already
     exists, so identical content is stored once),
   - upsert the `blobs` row and insert the `photos` row in SQLite,
   - compute the SigLIP2 embedding in-process and store it on the blob row
     (the vision tower runs inside the viewer through GoMLX; there is no
     separate inference service).
5. Mark the album `READY` and delete the staged ZIP.

ZIP format note:

- The worker downloads the whole object and uses `archive/zip` over the local
  file, so no S3 range support is required. The previous range-based reader
  (`internal/rangecache`) was removed with the index.json design.

------

## 8. UI Specification (Photo-first, Edge-to-Edge)

### 8.1 Page 1 — Random Wall (Immersive Masonry)

Principles:

- **No padding**. Content is edge-to-edge.
- Minimal UI overlays.
- Main action is scrolling and tapping photos.

Layout:

- Masonry/columns layout
- Gap: `0` (or extremely small if needed)
- Infinite scroll; loads next batch from `/api/feed`

Controls:

- **Bottom floating control bar** (safe-area aware), auto-hide on scroll:
  - Column selector: `1 | 2 | 3 | 4` (single tap)
  - Upload: `+`

Interaction:

- Tap photo -> navigate to Page 2:
  - Route: `/album/{albumId}?i={photoIndex}`

### 8.2 Page 2 — Album Viewer (Full-screen)

Principles:

- **One photo per screen**, edge-to-edge.
- Default fit mode: **cover** (fills screen; cropping allowed).
- Single tap toggles UI overlays.

Gestures:

- Swipe left/right: previous/next photo
- Swipe down: close (return to wall, restore scroll position)
- Pinch zoom: optional (if implemented)

Overlays (shown only when toggled on):

- Top: close/back + `currentIndex / total`
- Bottom: minimal progress indicator (optional)

------

## 9. Performance Strategy

- Store only:
  - **the staged ZIP** (transient; deleted after extraction)
  - **one object per distinct image content** (`blobs/<sha256>`)
- Identical images across albums cost one object, one blob row and one
  embedding.
- Per-album metadata is a few SQLite rows instead of an S3 object.
- Local disk cache (`CACHE_DIR`) serves repeated image views without touching
  S3.
- The recommendation index is held in memory and rebuilt from the catalog at
  startup.

------

## 10. Error Handling & Edge Cases

- ZIP contains non-images: ignore.
- Corrupt image: skip entry; record count accordingly.
- Empty album after filtering: mark finalize as failed (return error).
- Large ZIP: enforce max size and timeouts.
- Security:
  - Validate ZIP entry names (prevent path traversal if extracting)
  - Enforce allowed extensions and content-type sniffing

------

## 11. Implementation Milestones (Codex Task Breakdown)

1. **Backend**
   - S3 client, presigned PUT, upload finalize
   - Sequential zip download -> content-addressed `blobs/<sha256>` -> SQLite catalog
   - `/feed` sampling using the catalog
   - `/image/:albumId/:i` serving from blobs + disk cache
2. **Frontend**
   - Upload flow (select ZIP -> upload -> finalize -> ready)
   - Page 1 masonry wall (edge-to-edge) + infinite scroll + bottom bar
   - Page 2 full-screen viewer + swipe navigation + UI toggle + close restore
3. **Polish**
   - Caching, better error states, basic metrics/logging

