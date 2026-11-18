package claudehome

import (
	"bytes"
	"github.com/PHPCraftdream/cc-go-proxy/internal/claudehome/safefs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInterruptedPublicationRecoversAfterSourceReverts(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "agents", "reverted.md"), "original")
	write(t, filepath.Join(src, "agents", "edited.md"), "original edit")
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(dst, manifestFile))
	if err != nil {
		t.Fatal(err)
	}
	state := recoveryState{}
	for name, content := range map[string]string{
		"reverted.md": "published update",
		"new.md":      "published new file",
		"edited.md":   "published edit",
	} {
		file := filepath.Join(dst, "agents", name)
		write(t, file, content)
		digest, err := fileDigest(file)
		if err != nil {
			t.Fatal(err)
		}
		state["agents/"+name] = []string{digest}
	}
	if err := recordRecovery(dst, state, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate a later local edit and a separate local-only file.
	write(t, filepath.Join(dst, "agents", "edited.md"), "local edit")
	write(t, filepath.Join(dst, "agents", "local.md"), "local only")
	if err := os.Remove(filepath.Join(src, "agents", "edited.md")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dst, manifestFile))
	if err != nil || string(before) != string(manifest) {
		t.Fatalf("manifest changed before retry: %v", err)
	}
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dst, "agents", "reverted.md")); got != "original" {
		t.Fatalf("source revert not reconciled: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "agents", "new.md")); !os.IsNotExist(err) {
		t.Fatalf("interrupted new file not pruned: %v", err)
	}
	if got := read(t, filepath.Join(dst, "agents", "edited.md")); got != "local edit" {
		t.Fatalf("later edit lost: %q", got)
	}
	if got := read(t, filepath.Join(dst, "agents", "local.md")); got != "local only" {
		t.Fatalf("local-only file changed: %q", got)
	}
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dst, "agents", "edited.md")); got != "local edit" {
		t.Fatalf("later retry removed local edit: %q", got)
	}
}

func TestInterruptedBeforeReplacementPrunesOriginalManagedBytes(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "agents", "a.md"), "old a")
	write(t, filepath.Join(src, "agents", "b.md"), "old b")
	plugin := filepath.Join(src, "plugins", "installed.json")
	write(t, plugin, `{"path":"`+filepath.ToSlash(filepath.Join(src, "plugins"))+`"}`)
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	previous, _, err := loadManifestSync(dst)
	if err != nil {
		t.Fatal(err)
	}
	if previous["plugins/installed.json"].OutputHash == "" || previous["plugins/installed.json"].OutputHash == previous["plugins/installed.json"].Hash {
		t.Fatal("missing transformed plugin output digest")
	}
	write(t, filepath.Join(dst, "agents", "local.md"), "local edit")
	write(t, filepath.Join(src, "agents", "a.md"), "new a")
	var staged []*safefs.Staged
	for _, item := range []struct{ name, value string }{
		{"agents/a.md", "new a"},
		{"agents/b.md", "new b"},
		{"plugins/installed.json", `{"updated":true}`},
	} {
		file, err := safefs.Stage(filepath.Join(dst, filepath.FromSlash(item.name)), bytes.NewBufferString(item.value), nil)
		if err != nil {
			t.Fatal(err)
		}
		staged = append(staged, file)
		defer file.Discard()
	}
	if err := recordRecovery(dst, recoveryState{}, staged, previous); err != nil {
		t.Fatal(err)
	}
	if err := staged[0].Publish(false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Join(src, "agents", "b.md"), plugin} {
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agents/b.md", "plugins/installed.json"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Errorf("old managed file %s not pruned: %v", name, err)
		}
	}
	if got := read(t, filepath.Join(dst, "agents", "local.md")); got != "local edit" {
		t.Fatalf("local file changed: %q", got)
	}
}

func TestRecoveryRejectsUnsafePath(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	victim := filepath.Join(root, "victim")
	write(t, victim, "safe")
	write(t, filepath.Join(dst, recoveryFile), `{"../victim":["0000000000000000000000000000000000000000000000000000000000000000"]}`)
	if err := PrepareOwn(src, dst); err == nil {
		t.Fatal("accepted unsafe recovery path")
	}
	if got := read(t, victim); got != "safe" {
		t.Fatalf("victim changed: %q", got)
	}
}

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

func stagedFor(t *testing.T, dst, rel, content string) *safefs.Staged {
	t.Helper()
	target := filepath.Join(dst, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := safefs.Stage(target, strings.NewReader(content), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Discard)
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
	newHash, err := fileDigest(staged.Tmp)
	if err != nil {
		t.Fatal(err)
	}

	// The file still holds what the last preparation wrote: both are recorded.
	state := recoveryState{}
	previous := mirrored{"agents/a.md": {OutputHash: oldHash}}
	if err := recordRecovery(dst, state, []*safefs.Staged{staged}, previous); err != nil {
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
	if err := recordRecovery(dst, state, []*safefs.Staged{staged}, previous); err != nil {
		t.Fatal(err)
	}
	if got := state["agents/a.md"]; len(got) != 1 || got[0] != newHash {
		t.Errorf("state after an edit = %v, want [%s]", got, newHash)
	}
	// An untracked file has no earlier output to remember.
	state = recoveryState{}
	if err := recordRecovery(dst, state, []*safefs.Staged{staged}, mirrored{}); err != nil || len(state["agents/a.md"]) != 1 {
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
	staged.Dst = filepath.Join(dst, "..", "escape.md")
	if err := recordRecovery(dst, recoveryState{}, []*safefs.Staged{staged}, mirrored{}); err == nil {
		t.Fatal("a path outside the destination was recorded")
	}
}
