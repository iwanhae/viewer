package models

type PhotoMeta struct {
	I     int     `json:"i"`
	Name  string  `json:"name"`
	W     int     `json:"w"`
	H     int     `json:"h"`
	Ratio float64 `json:"ratio"`
}

type AlbumIndex struct {
	AlbumID          string      `json:"albumId"`
	OriginalFilename string      `json:"originalFilename"`
	CreatedAt        string      `json:"createdAt"`
	PhotoCount       int         `json:"photoCount"`
	Photos           []PhotoMeta `json:"photos"`
}

type AlbumSummary struct {
	AlbumID          string `json:"albumId"`
	OriginalFilename string `json:"originalFilename"`
	PhotoCount       int    `json:"photoCount"`
	CreatedAt        string `json:"createdAt"`
}

type FeedItem struct {
	AlbumID string  `json:"albumId"`
	I       int     `json:"i"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	Ratio   float64 `json:"ratio"`
	Src     string  `json:"src"`
}

type FeedResponse struct {
	Items      []FeedItem `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"`
}
