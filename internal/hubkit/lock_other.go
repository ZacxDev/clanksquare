//go:build !unix

package hubkit

import "errors"

var ErrLocked = errors.New("lock held")

// Lock is a no-op where flock is unavailable: overlapping runs are
// then not prevented, so schedule them so they cannot overlap.
func Lock(path string) (func(), error) { return func() {}, nil }
