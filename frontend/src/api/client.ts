export type FeedItem = {
  albumId: string
  i: number
  w: number
  h: number
  ratio: number
  src: string
}

export type FeedResponse = {
  items: FeedItem[]
  nextCursor?: string
}

export type PhotoMeta = {
  i: number
  name: string
  w: number
  h: number
  ratio: number
}

export type AlbumIndex = {
  albumId: string
  originalFilename: string
  createdAt: string
  photoCount: number
  photos: PhotoMeta[]
}

export type RecommendationItem = {
  albumId: string
  i: number
  w: number
  h: number
  score: number
  src: string
}

export type RecommendationResponse = {
  items: RecommendationItem[]
  status: 'ready' | 'partial' | 'pending'
}

type RawRecommendationResponse = {
  items: RecommendationItem[] | null
  status: 'ready' | 'partial' | 'pending'
}

export async function fetchFeed(params?: { cursor?: string; seed?: string }): Promise<FeedResponse> {
  const query = new URLSearchParams({ limit: '80' })
  if (params?.cursor) query.set('cursor', params.cursor)
  if (params?.seed) query.set('seed', params.seed)
  const res = await fetch(`/api/feed?${query.toString()}`)
  if (!res.ok) throw new Error(`feed failed: ${res.status}`)
  return (await res.json()) as FeedResponse
}

export async function createAlbum(file: File): Promise<{ albumId: string; upload: { method: string; url: string; headers: Record<string, string> } }> {
  const res = await fetch('/api/albums', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename: file.name, sizeBytes: file.size })
  })
  if (!res.ok) throw new Error(`create album failed: ${res.status}`)
  return (await res.json()) as { albumId: string; upload: { method: string; url: string; headers: Record<string, string> } }
}

export async function uploadZip(url: string, file: File, headers: Record<string, string>): Promise<void> {
  const res = await fetch(url, {
    method: 'PUT',
    headers,
    body: file
  })
  if (!res.ok) throw new Error(`zip upload failed: ${res.status}`)
}

export async function uploadZipFallback(albumId: string, file: File): Promise<void> {
  const form = new FormData()
  form.append('file', file)
  const res = await fetch(`/api/albums/${albumId}/upload`, {
    method: 'POST',
    body: form
  })
  if (!res.ok) throw new Error(`zip fallback upload failed: ${res.status}`)
}

export async function finalizeAlbum(albumId: string): Promise<{ status: string; photoCount: number }> {
  const res = await fetch(`/api/albums/${albumId}/finalize`, { method: 'POST' })
  if (!res.ok) throw new Error(`finalize failed: ${res.status}`)
  return (await res.json()) as { status: string; photoCount: number }
}

export async function fetchAlbum(albumId: string): Promise<AlbumIndex> {
  const res = await fetch(`/api/albums/${albumId}`)
  if (!res.ok) throw new Error(`fetch album failed: ${res.status}`)
  return (await res.json()) as AlbumIndex
}

export async function fetchRecommendations(
  albumId: string,
  index: number,
  limit = 12,
): Promise<RecommendationResponse> {
  const query = new URLSearchParams()
  query.set('limit', String(limit))
  const res = await fetch(`/api/recommendations/${albumId}/${index}?${query.toString()}`)
  if (!res.ok) throw new Error(`fetch recommendations failed: ${res.status}`)
  const data = (await res.json()) as RawRecommendationResponse
  return {
    status: data.status,
    items: Array.isArray(data.items) ? data.items : [],
  }
}
