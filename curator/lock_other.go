//go:build !unix

package curator

import "errors"

var errLocked = errors.New("lock held")

// lockState is a no-op where flock is unavailable: overlapping runs are
// then not prevented, so schedule them so they cannot overlap.
func lockState(path string) (func(), error) { return func() {}, nil }
