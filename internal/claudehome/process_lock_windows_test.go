package claudehome

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func expectContendedLock(f *os.File) error {
	var overlap windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlap)
	if err == nil {
		_ = unlockFile(f)
		return fmt.Errorf("destination lock unexpectedly available")
	}
	if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return fmt.Errorf("probe destination lock: %w", err)
	}
	return nil
}
