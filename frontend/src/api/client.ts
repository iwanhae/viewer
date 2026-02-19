import { cacheAlbum, getCachedAlbum as getCachedAlbumValue } from './albumCache'
import { ensureOK, requestJSON } from './http'
import type {
  AlbumIndex,
  AlbumStatus,
  AlbumStatusState,
  AlbumSearchResponse,
  FeedResponse,
  RecommendationItem,
  RecommendationResponse,
  RecommendationStatus,
} from './types'

export type {
  AlbumIndex,
  AlbumStatus,
  AlbumStatusState,
  AlbumIndexStatus,
  AlbumSearchItem,
  AlbumSearchResponse,
  FeedItem,
  FeedResponse,
  PhotoMeta,
  RecommendationItem,
  RecommendationResponse,
  RecommendationStatus,
} from './types'

export function getCachedAlbum(albumId: string): AlbumIndex | null {
  return getCachedAlbumValue(albumId)
}

export function seedCachedAlbum(album: AlbumIndex): void {
  cacheAlbum(album)
}

type RawRecommendationResponse = {
  items: RecommendationItem[] | null
  status: RecommendationStatus
}

export async function fetchFeed(params?: {
  cursor?: string
  seed?: string
  signal?: AbortSignal
}): Promise<FeedResponse> {
  const query = new URLSearchParams({ limit: '80' })
  if (params?.cursor) query.set('cursor', params.cursor)
  if (params?.seed) query.set('seed', params.seed)
  return await requestJSON<FeedResponse>(`/api/feed?${query.toString()}`, {
    signal: params?.signal,
  })
}

export async function createAlbum(file: File): Promise<{
  albumId: string
  upload: {
    strategy: string
    key: string
    partSizeBytes: number
    maxParts: number
  }
}> {
  return await requestJSON('/api/albums', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename: file.name, sizeBytes: file.size }),
  })
}

export async function initiateMultipartUpload(albumId: string, file: File): Promise<{
  uploadId: string
  partSizeBytes: number
  partCount: number
}> {
  return await requestJSON(`/api/albums/${albumId}/multipart/initiate`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      sizeBytes: file.size,
      contentType: file.type || 'application/zip',
    }),
  })
}

export async function presignMultipartPart(
  albumId: string,
  uploadId: string,
  partNumber: number,
): Promise<{ url: string; headers: Record<string, string> }> {
  return await requestJSON(`/api/albums/${albumId}/multipart/part-url`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ uploadId, partNumber }),
  })
}

export async function uploadMultipartPart(
  url: string,
  body: Blob,
  headers: Record<string, string>,
  signal?: AbortSignal,
): Promise<void> {
  await ensureOK(url, {
    method: 'PUT',
    headers,
    body,
    signal,
  })
}

export async function completeMultipartUpload(
  albumId: string,
  uploadId: string,
  parts: Array<{ partNumber: number; etag: string }> = [],
): Promise<{ status: string }> {
  return await requestJSON(`/api/albums/${albumId}/multipart/complete`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ uploadId, parts }),
  })
}

export async function abortMultipartUpload(
  albumId: string,
  uploadId: string,
): Promise<{ status: string }> {
  return await requestJSON(`/api/albums/${albumId}/multipart/abort`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ uploadId }),
  })
}

export async function finalizeAlbum(albumId: string): Promise<AlbumStatus> {
  return await requestJSON(`/api/albums/${albumId}/finalize`, { method: 'POST' })
}

export async function fetchAlbumStatus(albumId: string): Promise<AlbumStatus> {
  return await requestJSON(`/api/albums/${albumId}/status`)
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
    status: data.status,
    items: Array.isArray(data.items) ? data.items : [],
  }
}
