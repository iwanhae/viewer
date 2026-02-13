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

export async function fetchFeed(cursor?: string): Promise<FeedResponse> {
  const query = new URLSearchParams({ limit: '80' })
  if (cursor) query.set('cursor', cursor)
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
