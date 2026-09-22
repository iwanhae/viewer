export type FeedMode = 'random' | 'latest'

export type FeedItem = {
  albumId: string
  i: number
  // hash is the content-addressed blob key: image URLs are built from it
  // directly, no album lookup needed.
  hash: string
  w: number
  h: number
  ratio: number
}

export type FeedResponse = {
  items: FeedItem[]
  cursor?: string
  nextCursor?: string
  prevCursor?: string
  hasNext: boolean
  hasPrev: boolean
}

export type PhotoMeta = {
  i: number
  name: string
  hash: string
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

// AlbumCover points at the photo that represents an album: index 0.
export type AlbumCover = {
  i: number
  hash: string
  w: number
  h: number
  ratio: number
}

export type AlbumSearchItem = {
  albumId: string
  originalFilename: string
  photoCount: number
  createdAt: string
  sizeBytes: number
  cover?: AlbumCover
}

export type AlbumSearchResponse = {
  albums: AlbumSearchItem[]
}

export type RecommendationItem = {
	albumId: string
	i: number
	hash: string
  w: number
  h: number
  score: number
}

export type RecommendationResponse = {
  items: RecommendationItem[]
}

export type FinalizeStatus = 'QUEUED' | 'PROCESSING' | 'SUCCEEDED' | 'FAILED'

// EmbeddingProgress is the embedding coverage of a set of images: the whole
// catalog for the status endpoint, one album for a finalize response. ready to
// total is not the same as done when enabled is false: nothing is embedding
// then, and nothing ever will be.
export type EmbeddingProgress = {
  enabled: boolean
  active: boolean
  total: number
  ready: number
  failed: number
  pending: number
  // processing is the subset of pending currently leased by a worker.
  processing: number
  ratio: number
}

export type FinalizeResponse = {
  albumId: string
  status: FinalizeStatus
  photoCount?: number
  createdAt?: string
  error?: string
  updatedAt: string
  embedding?: EmbeddingProgress
}
