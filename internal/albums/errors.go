package albums

import "errors"

var (
	ErrAlbumNotFound       = errors.New("album not found")
	ErrAlbumSourceNotFound = errors.New("album source not found")
)
