import { cacheAlbum, getCachedAlbum as getCachedAlbumValue } from './albumCache'
import { ensureOK, requestJSON } from './http'
import type {
  AlbumIndex,
  AlbumSearchResponse,
  FeedMode,
  FeedResponse,
  FinalizeResponse,
  FinalizeStatus,
  RecommendationItem,
  RecommendationResponse,
} from './types'

export type {
  AlbumIndex,
  AlbumSearchItem,
  AlbumSearchResponse,
  FeedMode,
  FeedItem,
  FeedResponse,
  FinalizeResponse,
  FinalizeStatus,
  PhotoMeta,
  RecommendationItem,
  RecommendationResponse,
} from './types'

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
  signal?: AbortSignal
}): Promise<FeedResponse> {
  const query = new URLSearchParams({ limit: '40' })
  if (params?.mode) query.set('mode', params.mode)
  if (params?.seed) query.set('seed', params.seed)
  return await requestJSON<FeedResponse>(`/api/feed?${query.toString()}`, {
    signal: params?.signal,
  })
}

export async function createAlbum(file: File): Promise<{
  albumId: string
  uploadUrl: string
  uploadHeaders: Record<string, string>
  objectKey: string
}> {
  const created = await requestJSON<{
    albumId: string
    uploadUrl: string
    uploadHeaders?: Record<string, string>
    objectKey: string
  }>('/api/albums', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename: file.name, sizeBytes: file.size }),
  })
  return {
    albumId: created.albumId,
    uploadUrl: created.uploadUrl,
    uploadHeaders: created.uploadHeaders ?? {},
    objectKey: created.objectKey,
  }
}

export async function uploadAlbumObject(
  uploadURL: string,
  file: File,
  headers: Record<string, string>,
  signal?: AbortSignal,
): Promise<void> {
  await ensureOK(uploadURL, {
    method: 'PUT',
    headers,
    body: file,
    signal,
  })
}

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
