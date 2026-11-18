package claudehome

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/ktavfile"
)

const manifestFile = ".ccgp-mirror" + ktavfile.Ext

// mirrored is the one-way copy state: slash-separated path relative to the
// destination root -> source metadata and content hash at copy time.
type mirrored map[string]stamp

type stamp struct {
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
	Hash    string `json:"hash,omitempty"`
}

// manifestData is the manifest file: a list, since the keys are file paths
// and a Ktav key with a dot would read back as nesting.
type manifestData struct {
	Files []manifestEntry `json:"files"`
}

type manifestEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
	Hash    string `json:"hash,omitempty"`
}

// legacyManifest reads the .ccgp-mirror.json of earlier versions (path -> stamp).
func legacyManifest(data []byte, v any) error {
	var m mirrored
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*v.(*manifestData) = m.data()
	return nil
}

func (m mirrored) data() manifestData {
	d := manifestData{Files: make([]manifestEntry, 0, len(m))}
	for p, st := range m {
		d.Files = append(d.Files, manifestEntry{Path: p, Size: st.Size, ModTime: st.ModTime, Hash: st.Hash})
	}
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Path < d.Files[j].Path })
	return d
}

func loadManifest(dst string) mirrored {
	m := mirrored{}
	var d manifestData
	// A damaged manifest falls back to comparing destination contents.
	if err := ktavfile.Read(filepath.Join(dst, manifestFile), &d, legacyManifest); err == nil {
		for _, e := range d.Files {
			m[e.Path] = stamp{Size: e.Size, ModTime: e.ModTime, Hash: e.Hash}
		}
	}
	return m
}

func (m mirrored) save(dst string) error {
	s, err := ktavfile.Encode(m.data())
	if err != nil {
		return err
	}
	return writeIfChanged(filepath.Join(dst, manifestFile), []byte(s))
}

// isCredential reports files that must never be copied, anywhere in a tree.
func isCredential(name string) bool { return strings.EqualFold(name, credentialsFile) }

// mirrorTree copies the directory name under src into the same name under
// dst (one-way, official -> isolated). The source is never modified.
//
//   - New or changed source contents are mirrored.
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
		previous, tracked := man[key]
		hash, err := mirrorTreeFile(p, target, src, dst, name, rel, info, previous, tracked)
		if err != nil {
			return err
		}
		man[key] = stamp{Size: info.Size(), ModTime: info.ModTime().UnixNano(), Hash: hash}
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

func mirrorTreeFile(srcFile, target, src, dst, name, rel string, info fs.FileInfo, previous stamp, tracked bool) (string, error) {
	in, err := os.Open(srcFile)
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	plugin := name == "plugins" && filepath.Dir(rel) == "." && strings.HasSuffix(rel, ".json")
	var raw []byte
	h := sha256.New()
	if plugin {
		raw, err = io.ReadAll(in)
		if err != nil {
			return "", err
		}
		_, _ = h.Write(raw)
	} else if _, err := io.Copy(h, in); err != nil {
		return "", err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	st, statErr := os.Lstat(target)
	regular := statErr == nil && st.Mode().IsRegular()
	// Legacy stamps cannot distinguish a same-size, same-time source edit.
	legacyMatch := tracked && previous.Hash == "" && previous.Size == info.Size() && previous.ModTime == info.ModTime().UnixNano()
	if regular && (previous.Hash != "" && previous.Hash == hash || legacyMatch) {
		return hash, nil
	}
	var content io.Reader
	finalHash := hash
	if plugin {
		s := string(raw)
		srcPlug, dstPlug := filepath.Join(src, "plugins"), filepath.Join(dst, "plugins")
		for _, pair := range [][2]string{{jsonEscape(srcPlug), jsonEscape(dstPlug)}, {filepath.ToSlash(srcPlug), filepath.ToSlash(dstPlug)}, {srcPlug, dstPlug}} {
			s = strings.ReplaceAll(s, pair[0], pair[1])
		}
		b := []byte(s)
		sum := sha256.Sum256(b)
		finalHash = hex.EncodeToString(sum[:])
		content = bytes.NewReader(b)
	} else {
		if _, err := in.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		content = in
	}
	if regular {
		old, err := os.Open(target)
		if err != nil {
			return "", err
		}
		oldHash := sha256.New()
		_, err = io.Copy(oldHash, old)
		_ = old.Close()
		if err != nil {
			return "", err
		}
		if hex.EncodeToString(oldHash.Sum(nil)) == finalHash {
			return hash, nil
		}
	}
	mtime := info.ModTime()
	verified := &digestReader{reader: content, expected: finalHash, hash: sha256.New()}
	if err := publishFile(target, verified, &mtime, false); err != nil {
		return "", err
	}
	return hash, nil
}

type digestReader struct {
	reader   io.Reader
	hash     hash.Hash
	expected string
}

func (r *digestReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	_, _ = r.hash.Write(p[:n])
	if err == io.EOF && hex.EncodeToString(r.hash.Sum(nil)) != r.expected {
		return n, errors.New("claudehome: source changed while mirroring")
	}
	return n, err
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

func jsonEscape(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }
