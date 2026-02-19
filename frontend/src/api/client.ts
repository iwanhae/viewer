import { cacheAlbum, getCachedAlbum as getCachedAlbumValue } from './albumCache'
import { ensureOK, requestJSON } from './http'
import type {
  AlbumIndex,
  AlbumSearchResponse,
  FeedResponse,
  RecommendationItem,
  RecommendationResponse,
  RecommendationStatus,
} from './types'

export type {
  AlbumIndex,
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
  upload: { method: string; url: string; headers: Record<string, string> }
}> {
  return await requestJSON('/api/albums', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename: file.name, sizeBytes: file.size }),
  })
}

export async function uploadZip(url: string, file: File, headers: Record<string, string>): Promise<void> {
  await ensureOK(url, {
    method: 'PUT',
    headers,
    body: file,
  })
}

export async function uploadZipFallback(albumId: string, file: File): Promise<void> {
  const form = new FormData()
  form.append('file', file)
  await ensureOK(`/api/albums/${albumId}/upload`, {
    method: 'POST',
    body: form,
  })
}

export async function finalizeAlbum(albumId: string): Promise<{ status: string; photoCount: number }> {
  return await requestJSON(`/api/albums/${albumId}/finalize`, { method: 'POST' })
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
