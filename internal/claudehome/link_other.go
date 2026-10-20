//go:build !windows

package claudehome

import (
	"errors"
	"io/fs"
	"os"
)

// linkDir makes dst a symlink to src. It does nothing when src does not exist
// or dst already exists.
func linkDir(src, dst string) error {
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return nil
	}
	if _, err := os.Lstat(dst); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Symlink(src, dst)
}
