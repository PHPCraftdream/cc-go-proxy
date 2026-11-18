//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package dirlock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// LockFile takes the lock on f, waiting for it.
func LockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX) }

// UnlockFile releases the lock on f.
func UnlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }

// Held reports whether another holder has the lock, by trying to take it without waiting.
func Held(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		_ = UnlockFile(f)
		return false, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return true, nil
	}
	return false, err
}
