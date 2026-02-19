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

export type AlbumStatusState = 'queued' | 'processing' | 'ready' | 'failed'

export type AlbumStatus = {
  status: AlbumStatusState
  attempt: number
  lastError?: string
  photoCount: number
  updatedAt: string
}

export type AlbumIndexStatus = 'ready' | 'partial' | 'pending' | 'failed'

export type AlbumSearchItem = {
  albumId: string
  originalFilename: string
  photoCount: number
  createdAt: string
  indexStatus: AlbumIndexStatus
  indexedCount: number
  failedCount: number
  totalCount: number
}

export type AlbumSearchResponse = {
  albums: AlbumSearchItem[]
}

export type RecommendationItem = {
  albumId: string
  i: number
  w: number
  h: number
  score: number
  src: string
}

export type RecommendationStatus = 'ready' | 'partial' | 'pending' | 'failed'

export type RecommendationResponse = {
  items: RecommendationItem[]
  status: RecommendationStatus
}
