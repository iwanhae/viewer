import { cacheAlbum, getCachedAlbum as getCachedAlbumValue } from './albumCache'
import { requestJSON } from './http'
import type {
  AlbumIndex,
  AlbumSearchResponse,
  FeedMode,
  FeedResponse,
  FinalizeResponse,
  FinalizeStatus,
  PhotoSearchItem,
  PhotoSearchResponse,
  RecommendationItem,
  RecommendationResponse,
} from './types'

export type {
  AlbumCover,
  AlbumIndex,
  AlbumSearchItem,
  AlbumSearchResponse,
  FeedMode,
  FeedItem,
  FeedResponse,
  FinalizeResponse,
  FinalizeStatus,
  PhotoMeta,
  PhotoSearchItem,
  PhotoSearchResponse,
  RecommendationItem,
  RecommendationResponse,
} from './types'

// THUMBNAIL_WIDTH is the grid-tile width: every wall, album-grid and
// recommendation tile requests this scaled JPEG variant instead of the
// original. It must stay on the server's width ladder (320/640/1024); scaled
// responses carry an ETag derived from the bytes served.
export const THUMBNAIL_WIDTH = 640

// imageByHashUrl builds the URL for one image by its original upload hash. An optional
// width from the server's ladder (320/640/1024) asks for a scaled JPEG variant;
// without it the endpoint serves the current stored image.
export function imageByHashUrl(hash: string, width?: number): string {
  const base = `/api/image/${hash}`
  return width === undefined ? base : `${base}?w=${width}`
}

export function getCachedAlbum(albumId: string): AlbumIndex | null {
  return getCachedAlbumValue(albumId)
}

export function seedCachedAlbum(album: AlbumIndex): void {
  cacheAlbum(album)
}

type RawRecommendationResponse = {
  items: RecommendationItem[] | null
}

export async function fetchFeed(params?: {
  mode?: FeedMode
  seed?: string
  after?: string
  signal?: AbortSignal
}): Promise<FeedResponse> {
  const query = new URLSearchParams({ limit: '40' })
  if (params?.mode) query.set('mode', params.mode)
  if (params?.seed) query.set('seed', params.seed)
  if (params?.after) query.set('after', params.after)
  return await requestJSON<FeedResponse>(`/api/feed?${query.toString()}`, {
    signal: params?.signal,
  })
}

export async function createAlbum(file: File): Promise<{
  albumId: string
  uploadUrl: string
  objectKey: string
}> {
  return await requestJSON<{
    albumId: string
    uploadUrl: string
    objectKey: string
  }>('/api/albums', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename: file.name, sizeBytes: file.size }),
  })
}

// The PUT to the presigned URL lives in api/upload.ts: it needs XHR for
// upload progress, which the fetch helpers here do not provide.

export async function finalizeAlbum(albumId: string, options?: { signal?: AbortSignal }): Promise<FinalizeResponse> {
  return await requestJSON<FinalizeResponse>(`/api/albums/${albumId}/finalize`, {
    method: 'POST',
    signal: options?.signal,
  })
}

export async function fetchFinalizeStatus(
  albumId: string,
  options?: { signal?: AbortSignal },
): Promise<FinalizeResponse> {
  return await requestJSON<FinalizeResponse>(`/api/albums/${albumId}/finalize`, {
    signal: options?.signal,
  })
}

export async function fetchAlbum(albumId: string, options?: { signal?: AbortSignal }): Promise<AlbumIndex> {
  const album = await requestJSON<AlbumIndex>(`/api/albums/${albumId}`, { signal: options?.signal })
  cacheAlbum(album)
  return album
}

export async function fetchAlbumSearch(params?: {
  q?: string
  limit?: number
  signal?: AbortSignal
}): Promise<AlbumSearchResponse> {
  const query = new URLSearchParams()
  if (params?.q !== undefined) query.set('q', params.q)
  if (params?.limit !== undefined) query.set('limit', String(params.limit))

  const suffix = query.toString()
  const path = suffix ? `/api/albums/search?${suffix}` : '/api/albums/search'
  return await requestJSON<AlbumSearchResponse>(path, { signal: params?.signal })
}

// fetchPhotoSearch asks the backend for natural-language photo matches. The
// query is required (the empty case never leaves the hook), and the server
// answers with items already ranked best-first.
export async function fetchPhotoSearch(params: {
  q: string
  limit?: number
  signal?: AbortSignal
}): Promise<PhotoSearchResponse> {
  const query = new URLSearchParams()
  query.set('q', params.q)
  if (params?.limit !== undefined) query.set('limit', String(params.limit))

  return await requestJSON<PhotoSearchResponse>(`/api/photos/search?${query.toString()}`, {
    signal: params?.signal,
  })
}

// searchPhotosByImage uploads a photo from the user's device and asks the
// backend for visually similar catalog matches. The body is multipart with the
// file in the "image" field; no Content-Type header is set on purpose —
// requestJSON does not force one, so the browser generates the multipart
// boundary itself. Errors come back through the same typed ApiError as the
// JSON helpers, so the page can tell UNAVAILABLE (vector search off) from
// ordinary failures.
export async function searchPhotosByImage(params: {
  file: File
  limit?: number
  signal?: AbortSignal
}): Promise<PhotoSearchResponse> {
  const query = new URLSearchParams()
  if (params?.limit !== undefined) query.set('limit', String(params.limit))

  const body = new FormData()
  body.append('image', params.file)

  const suffix = query.toString()
  const path = suffix
    ? `/api/photos/search-by-image?${suffix}`
    : '/api/photos/search-by-image'
  return await requestJSON<PhotoSearchResponse>(path, {
    method: 'POST',
    body,
    signal: params?.signal,
  })
}

export async function fetchRecommendations(
  albumId: string,
  index: number,
  limit = 12,
  options?: { signal?: AbortSignal },
): Promise<RecommendationResponse> {
  const query = new URLSearchParams()
  query.set('limit', String(limit))
  const data = await requestJSON<RawRecommendationResponse>(
    `/api/recommendations/${albumId}/${index}?${query.toString()}`,
    { signal: options?.signal },
  )
  return {
    items: Array.isArray(data.items) ? data.items : [],
  }
}
