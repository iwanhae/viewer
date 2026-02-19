package storage

import "errors"

var ErrObjectNotFound = errors.New("object not found")

func IsNotFound(err error) bool {
	return errors.Is(err, ErrObjectNotFound)
}
