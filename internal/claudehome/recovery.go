package claudehome

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const recoveryFile = ".ccgp-mirror-pending.json"

type recoveryState map[string][]string

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func loadRecovery(dst string) (recoveryState, error) {
	file := filepath.Join(dst, recoveryFile)
	if err := unlink(file); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return recoveryState{}, nil
	}
	if err != nil {
		return nil, err
	}
	var state recoveryState
	if err := json.Unmarshal(data, &state); err != nil || state == nil {
		return nil, fmt.Errorf("claudehome: invalid recovery state")
	}
	paths := mirrored{}
	for key, hashes := range state {
		paths[key] = stamp{}
		if len(hashes) == 0 {
			return nil, fmt.Errorf("claudehome: empty recovery hashes for %q", key)
		}
		for _, hash := range hashes {
			decoded, err := hex.DecodeString(hash)
			if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != hash {
				return nil, fmt.Errorf("claudehome: invalid recovery hash for %q", key)
			}
		}
	}
	if err := validateManifest(paths); err != nil {
		return nil, err
	}
	return state, nil
}

// reconcileRecovery marks interrupted publications as owned, but not later edits.
func reconcileRecovery(dst string, state recoveryState, man mirrored) (map[string]bool, error) {
	protected := map[string]bool{}
	for key, hashes := range state {
		target := filepath.Join(dst, filepath.FromSlash(key))
		if err := safeRecoveryPath(dst, key); err != nil {
			return nil, err
		}
		info, err := os.Lstat(target)
		if errors.Is(err, fs.ErrNotExist) {
			delete(state, key)
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("claudehome: unsafe recovery target %s", target)
		}
		hash, err := fileDigest(target)
		if err != nil {
			return nil, err
		}
		matched := false
		for _, pending := range hashes {
			if hash == pending {
				matched = true
				break
			}
		}
		if matched {
			// Force content comparison instead of the old manifest fast path.
			man[key] = stamp{Hash: "pending"}
			state[key] = []string{hash}
		} else {
			protected[key] = true
			state[key] = state[key][:1]
		}
	}
	return protected, nil
}

func recordRecovery(dst string, state recoveryState, staged []*stagedFile, previous mirrored) error {
	for _, file := range staged {
		rel, err := filepath.Rel(dst, file.dst)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if err := validateManifest(mirrored{key: {}}); err != nil {
			return err
		}
		if old, tracked := previous[key]; tracked {
			expected := old.OutputHash
			if expected == "" {
				expected = old.Hash
			}
			if expected != "" {
				actual, err := fileDigest(file.dst)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				if err == nil && actual == expected {
					known := false
					for _, value := range state[key] {
						known = known || value == actual
					}
					if !known {
						state[key] = append(state[key], actual)
					}
				}
			}
		}
		hash, err := fileDigest(file.tmp)
		if err != nil {
			return err
		}
		found := false
		for _, previous := range state[key] {
			found = found || previous == hash
		}
		if !found {
			state[key] = append(state[key], hash)
		}
	}
	if len(state) == 0 {
		return nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeIfChanged(filepath.Join(dst, recoveryFile), data)
}
