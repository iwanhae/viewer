package images

import "errors"

var (
	ErrImageEntryNotFound = errors.New("image entry not found")
	ErrUnsupportedWidth   = errors.New("unsupported image width")
)
