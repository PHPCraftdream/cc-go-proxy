// Package safefs holds the file primitives of the profile mirror: a file is staged
// beside its destination and then made visible whole, and links and junctions
// are told from plain files.
package safefs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/securefile"
)

// Staged is a file written in full beside its destination, not yet visible there.
type Staged struct {
	Tmp string // the staged file
	Dst string // where Publish puts it
}

// Stage writes src, whole, beside dst; Publish makes it visible.
func Stage(dst string, src io.Reader, mtime *time.Time) (*Staged, error) {
	f, err := os.CreateTemp(filepath.Dir(dst), ".ccgp-stage-*")
	if err != nil {
		return nil, err
	}
	s := &Staged{Tmp: f.Name(), Dst: dst}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			s.Discard()
		}
	}()
	if err := securefile.RestrictToOwner(s.Tmp); err != nil {
		return nil, err
	}
	if _, err := io.Copy(f, src); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if mtime != nil {
		if err := os.Chtimes(s.Tmp, *mtime, *mtime); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}

// Discard removes the staged file.
func (s *Staged) Discard() { _ = os.Remove(s.Tmp) }

// Publish makes the staged file visible at its destination: it replaces what is
// there, or with noClobber leaves an existing file alone.
func (s *Staged) Publish(noClobber bool) error {
	if noClobber {
		err := os.Link(s.Tmp, s.Dst)
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return os.Rename(s.Tmp, s.Dst)
}

// PublishFile stages complete contents beside dst before making them visible.
func PublishFile(dst string, src io.Reader, mtime *time.Time, noClobber bool) error {
	s, err := Stage(dst, src, mtime)
	if err != nil {
		return err
	}
	defer s.Discard()
	return s.Publish(noClobber)
}
