package claudehome

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/securefile"
)

// publishFile stages complete contents beside dst before making them visible.
func publishFile(dst string, src io.Reader, mtime *time.Time, noClobber bool) error {
	f, err := os.CreateTemp(filepath.Dir(dst), ".ccgp-stage-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := securefile.RestrictToOwner(tmp); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if mtime != nil {
		if err := os.Chtimes(tmp, *mtime, *mtime); err != nil {
			return err
		}
	}
	if noClobber {
		err := os.Link(tmp, dst)
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return os.Rename(tmp, dst)
}
