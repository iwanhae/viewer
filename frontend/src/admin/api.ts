export type PipelineCounts = {
  blobCount: number
  photoCount: number
  albumCount: number
}

export type Overview = {
  measuredAt: string
  units: { counts: string; measuredAt: string }
  library: {
    albums: number
    photos: number
    blobs: number
    unreferencedBlobs: number
    missingBlobPhotos: number
  }
  encoding: {
    categories: Record<string, PipelineCounts>
    skippedReasons: Record<string, PipelineCounts>
    rawStates: Record<string, PipelineCounts>
  }
  embedding: {
    categories: Record<string, PipelineCounts>
    rawStates: Record<string, PipelineCounts>
  }
}

export type Photo = {
  albumId: string
  albumName: string
  idx: number
  name: string
  hash: string
  missingBlob: boolean
  width: number
  height: number
  sizeBytes: number | null
  contentType: string | null
  encodingRaw: string
  encoding: string
  encodingError: string
  embeddingRaw: string
  embedding: string
  embeddingError: string
  skippedReason: string
  blockReason: string
}

export type Blob = {
  hash: string
  sizeBytes: number
  contentType: string
  encodingRaw: string
  encoding: string
  encodingError: string
  embeddingRaw: string
  embedding: string
  embeddingError: string
  skippedReason: string
  blockReason: string
  photoCount: number
  albumCount: number
}

export type Album = {
  id: string
  originalFilename: string
  status: string
  error: string
  photoCount: number
}

export type AlbumDetail = Album & {
  encoding: {
    categories: Record<string, PipelineCounts>
    skippedReasons: Record<string, PipelineCounts>
    rawStates: Record<string, PipelineCounts>
  }
  embedding: {
    categories: Record<string, PipelineCounts>
    rawStates: Record<string, PipelineCounts>
  }
  missingBlobPhotos: number
}

export type Page<T> = { nextCursor: string | null } & (
  | { photos: T[] }
  | { blobs: T[] }
  | { albums: T[] }
)

export async function adminGet<T>(path: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(path, {
    method: 'GET',
    credentials: 'same-origin',
    cache: 'no-store',
    headers: { Accept: 'application/json' },
    signal
  })
  let body: unknown
  try {
    body = await response.json()
  } catch {
    throw new Error(`서버 응답을 읽을 수 없습니다 (HTTP ${response.status})`)
  }
  if (!response.ok) {
    const error = body as { error?: { message?: string } }
    throw new Error(error.error?.message || `요청 실패 (HTTP ${response.status})`)
  }
  return body as T
}

export function apiPath(resource: string, search = ''): string {
  return `/admin/api/${resource}${search}`
}

export function imagePath(hash: string): string {
  return `/api/image/${encodeURIComponent(hash)}?w=320`
}
