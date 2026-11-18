package claudehome

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const destinationLockFile = ".ccgp-mirror.lock"

// resolvedPath resolves existing ancestors without creating anything.
func resolvedPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		missing = append(missing, filepath.Base(path))
		path = parent
	}
}

func lockDestination(src, dst string) (func(), error) {
	source, err := resolvedPath(src)
	if err != nil {
		return nil, err
	}
	destination, err := resolvedPath(dst)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(source, destination)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("claudehome: destination is inside source: %s", dst)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dst, destinationLockFile)
	if st, err := os.Lstat(path); err == nil {
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("claudehome: unsafe lock file: %s", path)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (func(), error) {
		_ = f.Close()
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fail(err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return fail(fmt.Errorf("claudehome: unsafe lock file: %s", path))
	}
	if err := lockFile(f); err != nil {
		return fail(err)
	}
	current, err = os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		_ = unlockFile(f)
		return fail(fmt.Errorf("claudehome: lock file changed while waiting: %s", path))
	}
	// The lock file is intentionally never removed: waiters must lock the same object.
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
