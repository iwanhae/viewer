package storage

import "errors"

var ErrObjectNotFound = errors.New("object not found")
var ErrPreconditionFailed = errors.New("precondition failed")

func IsNotFound(err error) bool {
	return errors.Is(err, ErrObjectNotFound)
}

func IsPreconditionFailed(err error) bool {
	return errors.Is(err, ErrPreconditionFailed)
}
