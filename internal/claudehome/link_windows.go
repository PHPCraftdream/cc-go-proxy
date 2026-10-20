//go:build windows

package claudehome

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
)

// linkDir makes dst a junction to src (no admin rights needed). It does nothing
// when src does not exist or dst already exists.
func linkDir(src, dst string) error {
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return nil
	}
	if _, err := os.Lstat(dst); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", dst, src).CombinedOutput()
	if err != nil {
		return errors.New(string(out))
	}
	return nil
}
