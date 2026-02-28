export type FeedMode = 'random' | 'latest'

export type FeedItem = {
  albumId: string
  i: number
  w: number
  h: number
  ratio: number
}

export type FeedResponse = {
  items: FeedItem[]
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

export type AlbumSearchItem = {
  albumId: string
  originalFilename: string
  photoCount: number
  createdAt: string
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
}

export type RecommendationResponse = {
  items: RecommendationItem[]
}

export type FinalizeStatus = 'QUEUED' | 'PROCESSING' | 'SUCCEEDED' | 'FAILED'

export type FinalizeResponse = {
  albumId: string
  status: FinalizeStatus
  photoCount?: number
  createdAt?: string
  error?: string
  updatedAt: string
}
