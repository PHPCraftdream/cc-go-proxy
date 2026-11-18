package dirlock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// LockFile takes the lock on f, waiting for it.
func LockFile(f *os.File) error {
	var overlap windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlap)
}

// UnlockFile releases the lock on f.
func UnlockFile(f *os.File) error {
	var overlap windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlap)
}

// Held reports whether another holder has the lock, by trying to take it without waiting.
func Held(f *os.File) (bool, error) {
	var overlap windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlap)
	if err == nil {
		_ = UnlockFile(f)
		return false, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return true, nil
	}
	return false, err
}
