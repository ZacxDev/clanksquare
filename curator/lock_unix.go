//go:build unix

package curator

import (
	"errors"
	"os"
	"syscall"
)

// errLocked means another run holds the state lock.
var errLocked = errors.New("lock held")

// lockState takes an exclusive, non-blocking flock on path+".lock" and
// returns the function that releases it. The lock is on a sidecar file, not
// on the state file itself: Save replaces the state file by rename, so a
// lock on it would be left behind on the old inode and a second run would
// lock the new one.
func lockState(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() { f.Close() }, nil // closing the descriptor releases the lock
}
