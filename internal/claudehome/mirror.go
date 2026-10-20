package claudehome

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const manifestFile = ".ccgp-mirror.json"

// mirrored is the one-way copy state: slash-separated path relative to the
// destination root -> source size and modification time at copy time.
type mirrored map[string]stamp

type stamp struct {
	Size    int64 `json:"size"`
	ModTime int64 `json:"mtime"`
}

func loadManifest(dst string) mirrored {
	m := mirrored{}
	if b, err := os.ReadFile(filepath.Join(dst, manifestFile)); err == nil {
		_ = json.Unmarshal(b, &m) // a damaged manifest only costs a full re-copy
	}
	if m == nil {
		m = mirrored{}
	}
	return m
}

func (m mirrored) save(dst string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeIfChanged(filepath.Join(dst, manifestFile), b)
}

// isCredential reports files that must never be copied, anywhere in a tree.
func isCredential(name string) bool { return strings.EqualFold(name, credentialsFile) }

// mirrorTree copies the directory name under src into the same name under
// dst (one-way, official -> isolated). The source is never modified.
//
//   - New or changed files are copied (compared by size and mtime).
//   - Files copied by an earlier run that no longer exist at the source are
//     removed. Files created in the isolated dir by the session are kept.
//   - Credential files are never copied; symlinks are skipped.
func mirrorTree(src, dst, name string, man mirrored) error {
	srcRoot := filepath.Join(src, name)
	dstRoot := filepath.Join(dst, name)

	if err := unlink(dstRoot); err != nil {
		return err
	}
	// If the source itself is a link (e.g. dotfiles manager), read through it.
	resolved, err := filepath.EvalSymlinks(srcRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return pruneAll(dst, name, man)
	}
	if err != nil {
		return err
	}
	if st, err := os.Stat(resolved); err != nil || !st.IsDir() {
		return pruneAll(dst, name, man)
	}
	if err := os.MkdirAll(dstRoot, 0o700); err != nil {
		return err
	}
	if err := guardNotInside(dstRoot, resolved); err != nil {
		return err
	}

	seen := map[string]bool{}
	err = filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(resolved, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dstRoot, rel)
		switch {
		case d.IsDir():
			if err := unlink(target); err != nil {
				return err
			}
			return os.MkdirAll(target, 0o700)
		case !d.Type().IsRegular() || isCredential(d.Name()):
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		key := filepath.ToSlash(filepath.Join(name, rel))
		seen[key] = true
		want := stamp{info.Size(), info.ModTime().UnixNano()}
		if man[key] == want {
			if st, err := os.Stat(target); err == nil && st.Size() == want.Size {
				return nil
			}
		}
		if err := copyFile(p, target, info.ModTime()); err != nil {
			return err
		}
		man[key] = want
		return nil
	})
	if err != nil {
		return fmt.Errorf("mirror %s: %w", name, err)
	}

	// Prune what an earlier run copied and the source has since dropped.
	prefix := name + "/"
	for key := range man {
		if strings.HasPrefix(key, prefix) && !seen[key] {
			_ = os.Remove(filepath.Join(dst, filepath.FromSlash(key)))
			delete(man, key)
		}
	}
	removeEmptyDirs(dstRoot)
	return nil
}

// pruneAll removes everything an earlier run copied for name.
func pruneAll(dst, name string, man mirrored) error {
	prefix := name + "/"
	for key := range man {
		if strings.HasPrefix(key, prefix) {
			_ = os.Remove(filepath.Join(dst, filepath.FromSlash(key)))
			delete(man, key)
		}
	}
	removeEmptyDirs(filepath.Join(dst, name))
	return nil
}

// unlink removes path when it is a symlink or junction, never following it.
// Writing through such a link would modify the user's own directory.
func unlink(path string) error {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
		// Some Go versions report a Windows junction as a plain directory:
		// if the path resolves elsewhere, drop it (an empty real dir goes too,
		// a non-empty one stays and is stopped by guardNotInside).
		if abs, aerr := filepath.Abs(path); aerr == nil {
			if res, rerr := filepath.EvalSymlinks(path); rerr == nil && !strings.EqualFold(filepath.Clean(res), filepath.Clean(abs)) {
				_ = os.Remove(path)
			}
		}
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove link %s: %w", path, err)
	}
	return nil
}

// guardNotInside refuses to mirror when dst resolves into the source tree.
func guardNotInside(dst, resolvedSrc string) error {
	d, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(resolvedSrc, d)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("claudehome: %s resolves into the source tree %s", dst, resolvedSrc)
	}
	return nil
}

func copyFile(src, dst string, mtime time.Time) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".ccgp-tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chtimes(tmp, mtime, mtime); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// removeEmptyDirs deletes empty directories below root (root itself stays).
func removeEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // fails (and is kept) when not empty
	}
}

// rewritePluginPaths points the absolute paths stored in the mirrored plugin
// metadata (plugins/*.json) at the isolated copy instead of the source.
func rewritePluginPaths(src, dst string) error {
	srcPlug, dstPlug := filepath.Join(src, "plugins"), filepath.Join(dst, "plugins")
	entries, err := os.ReadDir(dstPlug)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pairs := [][2]string{
		{jsonEscape(srcPlug), jsonEscape(dstPlug)},
		{filepath.ToSlash(srcPlug), filepath.ToSlash(dstPlug)},
		{srcPlug, dstPlug},
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dstPlug, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := string(b)
		for _, pr := range pairs {
			s = strings.ReplaceAll(s, pr[0], pr[1])
		}
		if err := writeIfChanged(p, []byte(s)); err != nil {
			return err
		}
	}
	return nil
}

func jsonEscape(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }
