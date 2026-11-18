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

type stagedFile struct {
	tmp string
	dst string
}

func stageFile(dst string, src io.Reader, mtime *time.Time) (*stagedFile, error) {
	f, err := os.CreateTemp(filepath.Dir(dst), ".ccgp-stage-*")
	if err != nil {
		return nil, err
	}
	s := &stagedFile{tmp: f.Name(), dst: dst}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			s.discard()
		}
	}()
	if err := securefile.RestrictToOwner(s.tmp); err != nil {
		return nil, err
	}
	if _, err := io.Copy(f, src); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if mtime != nil {
		if err := os.Chtimes(s.tmp, *mtime, *mtime); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}

func (s *stagedFile) discard() { _ = os.Remove(s.tmp) }

func (s *stagedFile) publish(noClobber bool) error {
	if noClobber {
		err := os.Link(s.tmp, s.dst)
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return os.Rename(s.tmp, s.dst)
}

// publishFile stages complete contents beside dst before making them visible.
func publishFile(dst string, src io.Reader, mtime *time.Time, noClobber bool) error {
	s, err := stageFile(dst, src, mtime)
	if err != nil {
		return err
	}
	defer s.discard()
	return s.publish(noClobber)
}
