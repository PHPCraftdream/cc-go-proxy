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
	"path"
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
	Size       int64  `json:"size"`
	ModTime    int64  `json:"mtime"`
	Hash       string `json:"hash,omitempty"`
	OutputHash string `json:"outputHash,omitempty"`
}

// manifestData is the manifest file: a list, since the keys are file paths
// and a Ktav key with a dot would read back as nesting.
type manifestData struct {
	Files []manifestEntry `json:"files"`
}

type manifestEntry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	ModTime    int64  `json:"mtime"`
	Hash       string `json:"hash,omitempty"`
	OutputHash string `json:"outputHash,omitempty"`
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
		d.Files = append(d.Files, manifestEntry{Path: p, Size: st.Size, ModTime: st.ModTime, Hash: st.Hash, OutputHash: st.OutputHash})
	}
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Path < d.Files[j].Path })
	return d
}

// loadManifestSync reads legacy state without migrating it during preparation.
func loadManifestSync(dst string) (mirrored, bool, error) {
	name := filepath.Join(dst, manifestFile)
	legacy := ktavfile.LegacyPath(name)
	for _, p := range []string{name, legacy} {
		if err := unlink(p); err != nil {
			return nil, false, err
		}
	}
	m := mirrored{}
	var d manifestData
	_, err := os.ReadFile(name)
	if err == nil {
		if ktavfile.Read(name, &d, nil) != nil {
			return m, false, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	} else {
		data, readErr := os.ReadFile(legacy)
		if errors.Is(readErr, fs.ErrNotExist) {
			return m, false, nil
		}
		if readErr != nil {
			return nil, false, readErr
		}
		if legacyManifest(data, &d) != nil {
			return m, false, nil
		}
		for _, e := range d.Files {
			m[e.Path] = stamp{Size: e.Size, ModTime: e.ModTime, Hash: e.Hash, OutputHash: e.OutputHash}
		}
		return m, true, nil
	}
	for _, e := range d.Files {
		m[e.Path] = stamp{Size: e.Size, ModTime: e.ModTime, Hash: e.Hash, OutputHash: e.OutputHash}
	}
	return m, false, nil
}

func (m mirrored) save(dst string) error {
	s, err := ktavfile.Encode(m.data())
	if err != nil {
		return err
	}
	return writeIfChanged(filepath.Join(dst, manifestFile), []byte(s))
}

func planTopLevel(src, dst, name string, man mirrored, seen map[string]bool, staged *[]*stagedFile) error {
	source := filepath.Join(src, name)
	data, err := os.ReadFile(source)
	if errors.Is(err, fs.ErrNotExist) {
		if _, statErr := os.Lstat(source); errors.Is(statErr, fs.ErrNotExist) {
			return nil
		} else if statErr != nil {
			return statErr
		}
		return err
	}
	if err != nil {
		return err
	}
	if name == "settings.json" {
		data, err = SanitizeSettings(data)
		if err != nil {
			return fmt.Errorf("claudehome: %s: %w", filepath.Join(src, name), err)
		}
	}
	target := filepath.Join(dst, name)
	if err := unlink(target); err != nil {
		return err
	}
	if info, statErr := os.Lstat(target); statErr == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("claudehome: target is not a regular file: %s", target)
		}
		old, readErr := os.ReadFile(target)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(old, data) {
			file, err := stageFile(target, bytes.NewReader(data), nil)
			if err != nil {
				return err
			}
			*staged = append(*staged, file)
		}
	} else if errors.Is(statErr, fs.ErrNotExist) {
		file, err := stageFile(target, bytes.NewReader(data), nil)
		if err != nil {
			return err
		}
		*staged = append(*staged, file)
	} else {
		return statErr
	}
	sum := sha256.Sum256(data)
	man[name] = stamp{Size: int64(len(data)), Hash: hex.EncodeToString(sum[:]), OutputHash: hex.EncodeToString(sum[:])}
	seen[name] = true
	return nil
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
func mirrorTree(src, dst, name string, man mirrored, seen map[string]bool, staged *[]*stagedFile) error {
	srcRoot := filepath.Join(src, name)
	dstRoot := filepath.Join(dst, name)

	if err := unlink(dstRoot); err != nil {
		return err
	}
	// If the source itself is a link (e.g. dotfiles manager), read through it.
	resolved, err := filepath.EvalSymlinks(srcRoot)
	if errors.Is(err, fs.ErrNotExist) {
		if _, statErr := os.Lstat(srcRoot); errors.Is(statErr, fs.ErrNotExist) {
			return nil
		} else if statErr != nil {
			return statErr
		}
		return err
	}
	if err != nil {
		return err
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("mirror source is not a directory: %s", srcRoot)
	}
	if err := os.MkdirAll(dstRoot, 0o700); err != nil {
		return err
	}
	if err := guardNotInside(dstRoot, resolved); err != nil {
		return err
	}

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
			key := filepath.ToSlash(filepath.Join(name, rel))
			for previous := range man {
				if previous == key || strings.HasPrefix(previous, key+"/") {
					seen[previous] = true
				}
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		key := filepath.ToSlash(filepath.Join(name, rel))
		seen[key] = true
		previous, tracked := man[key]
		if err := unlink(target); err != nil {
			return err
		}
		var outputHash string
		hash, err := mirrorTreeFile(p, target, src, dst, name, rel, info, previous, tracked, staged, &outputHash)
		if err != nil {
			return err
		}
		man[key] = stamp{Size: info.Size(), ModTime: info.ModTime().UnixNano(), Hash: hash, OutputHash: outputHash}
		return nil
	})
	if err != nil {
		return fmt.Errorf("mirror %s: %w", name, err)
	}

	return nil
}

func validateManifest(man mirrored) error {
	for key := range man {
		parts := strings.Split(key, "/")
		validRoot := false
		for _, name := range mirroredDirs {
			if parts[0] == name {
				validRoot = true
			}
		}
		if len(parts) == 1 {
			validRoot = key == "settings.json"
			for _, name := range mirroredFiles {
				validRoot = validRoot || key == name
			}
		}
		if !validRoot || path.Clean(key) != key || strings.Contains(key, `\`) || !filepath.IsLocal(filepath.FromSlash(key)) {
			return fmt.Errorf("claudehome: unsafe manifest path %q", key)
		}
		for _, part := range parts {
			if part == "" || part == "." || part == ".." || isCredential(part) {
				return fmt.Errorf("claudehome: unsafe manifest path %q", key)
			}
		}
	}
	return nil
}

// pruneStale only removes previously tracked files, after all copies succeed.
func pruneStale(dst string, man mirrored, seen map[string]bool) error {
	var stale []string
	for key := range man {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	root, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return err
	}
	for _, key := range stale {
		if err := safeRecoveryPath(dst, key); err != nil {
			return err
		}
	}
	for _, key := range stale {
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(key))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		delete(man, key)
	}
	return nil
}

func safeRecoveryPath(dst, key string) error {
	current, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return err
	}
	parts := strings.Split(key, "/")
	for i, part := range parts {
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(current)
		if err != nil {
			return err
		}
		if !strings.EqualFold(filepath.Clean(resolved), filepath.Clean(current)) || st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || (i < len(parts)-1 && !st.IsDir()) || (i == len(parts)-1 && !st.Mode().IsRegular()) {
			return fmt.Errorf("claudehome: unsafe managed path %s", current)
		}
	}
	return nil
}

// unlink refuses links and junctions rather than following or removing them.
func unlink(path string) error {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("claudehome: refusing link %s", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Clean(resolved), filepath.Clean(abs)) {
		return fmt.Errorf("claudehome: refusing redirected path %s", path)
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

func mirrorTreeFile(srcFile, target, src, dst, name, rel string, info fs.FileInfo, previous stamp, tracked bool, staged *[]*stagedFile, outputHash *string) (string, error) {
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
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return "", statErr
	}
	regular := statErr == nil && st.Mode().IsRegular()
	if statErr == nil && !regular {
		return "", fmt.Errorf("claudehome: target is not a regular file: %s", target)
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
	*outputHash = finalHash
	// Legacy stamps cannot distinguish a same-size, same-time source edit.
	legacyMatch := tracked && previous.Hash == "" && previous.Size == info.Size() && previous.ModTime == info.ModTime().UnixNano()
	if regular && (previous.Hash != "" && previous.Hash == hash || legacyMatch) {
		return hash, nil
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
	file, err := stageFile(target, verified, &mtime)
	if err != nil {
		return "", err
	}
	*staged = append(*staged, file)
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

func jsonEscape(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }
