package models

type PhotoMeta struct {
	I     int     `json:"i"`
	Name  string  `json:"name"`
	Hash  string  `json:"hash"`
	W     int     `json:"w"`
	H     int     `json:"h"`
	Ratio float64 `json:"ratio"`
}

// AlbumPhotoCount identifies a ready album together with its photo count. It
// is the whole album view the random feed needs: enough to sample an album and
// a photo index, after which the photo row itself is fetched directly.
type AlbumPhotoCount struct {
	AlbumID    string
	PhotoCount int
}

type AlbumIndex struct {
	AlbumID          string      `json:"albumId"`
	OriginalFilename string      `json:"originalFilename"`
	CreatedAt        string      `json:"createdAt"`
	PhotoCount       int         `json:"photoCount"`
	Photos           []PhotoMeta `json:"photos"`
}

// AlbumCover points at the image that represents an album: the photo at
// index 0. Ratio is width/height, denormalized from the photo row.
type AlbumCover struct {
	I     int     `json:"i"`
	Hash  string  `json:"hash"`
	W     int     `json:"w"`
	H     int     `json:"h"`
	Ratio float64 `json:"ratio"`
}

type AlbumSearchItem struct {
	AlbumID          string      `json:"albumId"`
	OriginalFilename string      `json:"originalFilename"`
	PhotoCount       int         `json:"photoCount"`
	CreatedAt        string      `json:"createdAt"`
	SizeBytes        int64       `json:"sizeBytes"`
	Cover            *AlbumCover `json:"cover,omitempty"`
}

type FeedItem struct {
	AlbumID string  `json:"albumId"`
	I       int     `json:"i"`
	Hash    string  `json:"hash"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	Ratio   float64 `json:"ratio"`
}

// AlbumCoverEntry is a ready album with its cover photo. The latest feed ranks
// and pages over albums, so it only ever needs one photo per album - never the
// other rows.
type AlbumCoverEntry struct {
	AlbumID   string    `json:"albumId"`
	CreatedAt string    `json:"createdAt"`
	Cover     PhotoMeta `json:"cover"`
}

type FeedResponse struct {
	Items      []FeedItem `json:"items"`
	Cursor     string     `json:"cursor,omitempty"`
	NextCursor string     `json:"nextCursor,omitempty"`
	PrevCursor string     `json:"prevCursor,omitempty"`
	HasNext    bool       `json:"hasNext"`
	HasPrev    bool       `json:"hasPrev"`
}
