package images

import "errors"

var (
	ErrPhotoIndexOutOfRange = errors.New("photo index out of range")
	ErrImageEntryNotFound   = errors.New("image entry not found")
)
