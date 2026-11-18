package claudehome

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var zeroHash = strings.Repeat("0", 64)

func TestLoadRecoveryValidation(t *testing.T) {
	missing, err := loadRecovery(t.TempDir())
	if err != nil || len(missing) != 0 {
		t.Fatalf("no file: %v, %v", missing, err)
	}
	valid, err := func() (recoveryState, error) {
		dst := t.TempDir()
		write(t, filepath.Join(dst, recoveryFile), `{"agents/a.md":["`+zeroHash+`"]}`)
		return loadRecovery(dst)
	}()
	if err != nil || len(valid["agents/a.md"]) != 1 {
		t.Fatalf("valid state: %v, %v", valid, err)
	}
	for name, body := range map[string]string{
		"not json":         `not json`,
		"null":             `null`,
		"empty hashes":     `{"agents/a.md":[]}`,
		"short hash":       `{"agents/a.md":["abc"]}`,
		"not hex":          `{"agents/a.md":["` + strings.Repeat("z", 64) + `"]}`,
		"upper-case hash":  `{"agents/a.md":["` + strings.Repeat("A", 64) + `"]}`,
		"credentials":      `{"agents/.credentials.json":["` + zeroHash + `"]}`,
		"top-level cred":   `{".credentials.json":["` + zeroHash + `"]}`,
		"not a mirror dir": `{"other/a.md":["` + zeroHash + `"]}`,
		"escapes":          `{"agents/../../x":["` + zeroHash + `"]}`,
		"a bare dir":       `{"agents":["` + zeroHash + `"]}`,
	} {
		dst := t.TempDir()
		write(t, filepath.Join(dst, recoveryFile), body)
		if state, err := loadRecovery(dst); err == nil {
			t.Errorf("%s accepted: %v", name, state)
		}
	}
}

func TestLoadRecoveryRefusesALinkedStateFile(t *testing.T) {
	dst := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	write(t, target, `{"agents/a.md":["`+zeroHash+`"]}`)
	if err := os.Symlink(target, filepath.Join(dst, recoveryFile)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := loadRecovery(dst); err == nil {
		t.Fatal("a symlinked recovery file was read")
	}
}

func TestReconcileRecoveryClassifiesTargets(t *testing.T) {
	dst := t.TempDir()
	write(t, filepath.Join(dst, "agents", "published.md"), "published")
	write(t, filepath.Join(dst, "agents", "edited.md"), "edited later")
	published, err := fileDigest(filepath.Join(dst, "agents", "published.md"))
	if err != nil {
		t.Fatal(err)
	}
	state := recoveryState{
		"agents/published.md": {zeroHash, published},
		"agents/edited.md":    {zeroHash, strings.Repeat("1", 64)},
		"agents/gone.md":      {zeroHash},
	}
	man := mirrored{"agents/published.md": {Hash: "old"}}
	protected, err := reconcileRecovery(dst, state, man)
	if err != nil {
		t.Fatal(err)
	}
	if man["agents/published.md"].Hash != "pending" || protected["agents/published.md"] {
		t.Errorf("a published file must be re-compared, not protected: %+v, %v", man["agents/published.md"], protected)
	}
	if got := state["agents/published.md"]; len(got) != 1 || got[0] != published {
		t.Errorf("published state = %v", got)
	}
	if !protected["agents/edited.md"] || len(state["agents/edited.md"]) != 1 {
		t.Errorf("a locally edited file must be protected: %v %v", protected, state["agents/edited.md"])
	}
	if _, ok := state["agents/gone.md"]; ok || protected["agents/gone.md"] {
		t.Error("a file that is gone must be forgotten")
	}
}

func TestReconcileRecoveryRefusesANonFileTarget(t *testing.T) {
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, "agents", "a.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileRecovery(dst, recoveryState{"agents/a.md": {zeroHash}}, mirrored{}); err == nil {
		t.Fatal("a directory was treated as a recoverable file")
	}
}

func stagedFor(t *testing.T, dst, rel, content string) *stagedFile {
	t.Helper()
	target := filepath.Join(dst, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := stageFile(target, strings.NewReader(content), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.discard)
	return s
}

func TestRecordRecoveryKeepsTheReplacedAndTheNewContent(t *testing.T) {
	dst := t.TempDir()
	write(t, filepath.Join(dst, "agents", "a.md"), "old output")
	oldHash, err := fileDigest(filepath.Join(dst, "agents", "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	staged := stagedFor(t, dst, "agents/a.md", "new output")
	newHash, err := fileDigest(staged.tmp)
	if err != nil {
		t.Fatal(err)
	}

	// The file still holds what the last preparation wrote: both are recorded.
	state := recoveryState{}
	previous := mirrored{"agents/a.md": {OutputHash: oldHash}}
	if err := recordRecovery(dst, state, []*stagedFile{staged}, previous); err != nil {
		t.Fatal(err)
	}
	if got := state["agents/a.md"]; len(got) != 2 || got[0] != oldHash || got[1] != newHash {
		t.Errorf("state = %v, want [%s %s]", got, oldHash, newHash)
	}
	data, err := os.ReadFile(filepath.Join(dst, recoveryFile))
	if err != nil || !bytes.Contains(data, []byte(newHash)) {
		t.Fatalf("recovery file: %q, %v", data, err)
	}
	if _, err := loadRecovery(dst); err != nil {
		t.Errorf("the written state does not load back: %v", err)
	}

	// The file was edited since: only the new content is recorded.
	write(t, filepath.Join(dst, "agents", "a.md"), "edited by the session")
	state = recoveryState{}
	if err := recordRecovery(dst, state, []*stagedFile{staged}, previous); err != nil {
		t.Fatal(err)
	}
	if got := state["agents/a.md"]; len(got) != 1 || got[0] != newHash {
		t.Errorf("state after an edit = %v, want [%s]", got, newHash)
	}
	// An untracked file has no earlier output to remember.
	state = recoveryState{}
	if err := recordRecovery(dst, state, []*stagedFile{staged}, mirrored{}); err != nil || len(state["agents/a.md"]) != 1 {
		t.Errorf("untracked: %v, %v", state, err)
	}
}

func TestRecordRecoveryWritesNothingWithoutWork(t *testing.T) {
	dst := t.TempDir()
	if err := recordRecovery(dst, recoveryState{}, nil, mirrored{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, recoveryFile)); !os.IsNotExist(err) {
		t.Errorf("a recovery file was written for nothing: %v", err)
	}
}

func TestRecordRecoveryRefusesAnUnsafeKey(t *testing.T) {
	dst := t.TempDir()
	staged := stagedFor(t, dst, "agents/a.md", "x")
	staged.dst = filepath.Join(dst, "..", "escape.md")
	if err := recordRecovery(dst, recoveryState{}, []*stagedFile{staged}, mirrored{}); err == nil {
		t.Fatal("a path outside the destination was recorded")
	}
}
