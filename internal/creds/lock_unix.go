//go:build linux || darwin || freebsd

package creds

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLockFile(f *os.File) (func() error, bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return func() error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }, true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return nil, false, nil
	}
	return nil, false, err
}
