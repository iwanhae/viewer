# Viewer — Final Design (Go + TypeScript)

## 1. Goals

1. View a large personal photo archive organized as **ZIP albums** (≈10k ZIPs, ~20 photos each).
2. Keep storage efficient on a **home S3-compatible server** that performs poorly with many small objects.
3. Provide a **photo-centric UI** with:
   - **Page 1:** Random “Pinterest-like” wall (edge-to-edge)
   - **Page 2:** Full-screen album viewer (edge-to-edge)
4. Avoid a database. Use **S3 as source of truth** + **one metadata JSON per ZIP**.

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
   - Generate and store album metadata JSON
   - Random feed API
   - Image serving API (extract from ZIP; optional resize/caching)
3. **S3-Compatible Storage (Home Server)**
   - Stores ZIP objects (large, few)
   - Stores per-album `index.json` (small, few)
4. **Optional Local Disk Cache (API Server)**
   - Cache resized images and/or extracted bytes to reduce repeated ZIP work

------

## 4. Storage Layout (S3 Keys)

Bucket: `photo-archive`

For each album (`albumId` is a UUID or content-hash):

- Source ZIP (1 object):
  - `albums/{albumId}/source.zip`
- Album metadata (1 object):
  - `albums/{albumId}/index.json`

No per-photo thumbnail objects in S3.

------

## 5. Album Metadata Format (`index.json`)

Purpose:

- Allow the UI and APIs to:
  - list photos in order
  - compute masonry layout using aspect ratio
  - address a photo by `(albumId, photoIndex)`

Example:

```json
{
  "albumId": "a1b2c3...",
  "originalFilename": "2021_trip.zip",
  "createdAt": "2026-02-13T00:00:00Z",
  "photoCount": 20,
  "photos": [
    {
      "i": 0,
      "name": "IMG_0001.JPG",
      "w": 4032,
      "h": 3024,
      "ratio": 1.3333
    }
  ]
}
```

Notes:

- `i` is the stable ordering index (sort by filename or ZIP entry order; pick one and keep it consistent).
- Width/height are extracted from image headers (or by decoding if needed).

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

- Server verifies ZIP exists in S3.
- Server builds `albums/{albumId}/index.json`.
- For MVP: do it synchronously (may take seconds). Optionally return early and allow polling.

### 6.3 Get Album Metadata

```
GET /api/albums/{albumId}
```

Response: the `index.json` content.

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

- List `albums/*/index.json` objects (or list `albums/` prefixes and fetch index.json per album; cache results server-side).

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

1. Download ZIP to local temp **or** read via Range (optional optimization).
2. Enumerate entries; keep only `.jpg/.jpeg/.png`.
3. Determine ordering (e.g., filename sort).
4. Extract width/height for each entry:
   - Prefer header parsing or minimal decode.
5. Write `index.json` to S3.

ZIP format note:

- Many implementations locate the “central directory” at the end of the ZIP to enumerate entries efficiently, which can enable Range-based partial reads. ([Rhardih](https://rhardih.io/tag/zip/?utm_source=chatgpt.com))

### 7.2 Range-based ZIP Reading (Optional Optimization)

If the home S3 server supports HTTP Range well:

- Fetch the last chunk to find the End-of-Central-Directory
- Fetch the central directory region
- Fetch only the bytes for requested entries

This can avoid full ZIP downloads, but is not required for MVP.

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

## 9. Performance Strategy (No Small S3 Objects)

- Store only:
  - **1 ZIP object per album**
  - **1 JSON index per album**
- Avoid storing thumbnails per photo in S3.
- Optional server disk cache for resized images:
  - Improves repeated viewing without adding S3 objects.
- Server should cache `index.json` in memory to avoid repeated S3 reads.

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
   - ZIP indexing -> generate `index.json` -> put to S3
   - `/feed` sampling using cached indices
   - `/image/:albumId/:i` serving + optional resize + optional disk cache
2. **Frontend**
   - Upload flow (select ZIP -> upload -> finalize -> ready)
   - Page 1 masonry wall (edge-to-edge) + infinite scroll + bottom bar
   - Page 2 full-screen viewer + swipe navigation + UI toggle + close restore
3. **Polish**
   - Caching, better error states, basic metrics/logging

