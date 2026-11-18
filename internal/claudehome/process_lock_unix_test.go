//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package claudehome

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func expectContendedLock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		_ = unlockFile(f)
		return fmt.Errorf("destination lock unexpectedly available")
	}
	if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
		return fmt.Errorf("probe destination lock: %w", err)
	}
	return nil
}
