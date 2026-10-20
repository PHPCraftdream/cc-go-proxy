// Package securefile writes secrets atomically, readable only by the current user.
package securefile

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile writes data via temp file in the same dir: restrict, write, fsync, rename.
func WriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after successful rename

	if err := RestrictToOwner(tmp); err != nil {
		_ = f.Close()
		return fmt.Errorf("restrict %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
