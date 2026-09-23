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

// PhotoSearchItem is one natural-language photo-search hit: the album and the
// zero-based photo index to navigate to, the hash to render the thumbnail
// from, and the ranking score the server ordered the items by.
export type PhotoSearchItem = {
  albumId: string
  i: number
  hash: string
  w: number
  h: number
  score: number
}

export type PhotoSearchResponse = {
  items: PhotoSearchItem[]
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

export type FinalizeResponse = {
  albumId: string
  status: FinalizeStatus
  error?: string
  updatedAt: string
}
