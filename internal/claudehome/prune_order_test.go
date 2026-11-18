package claudehome

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLateMirrorFailureKeepsStaleFilesAndManifest(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	oldSource := filepath.Join(src, "agents", "old.md")
	write(t, oldSource, "old")
	write(t, filepath.Join(src, "CLAUDE.md"), "before")
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dst, manifestFile)
	before, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldSource); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "plugins"), "not a directory")
	write(t, filepath.Join(src, "CLAUDE.md"), "after")
	if err := PrepareOwn(src, dst); err == nil {
		t.Fatal("accepted non-directory source")
	}
	if got := read(t, filepath.Join(dst, "CLAUDE.md")); got != "before" {
		t.Fatalf("early publication on failed preparation: %q", got)
	}
	stages, err := filepath.Glob(filepath.Join(dst, ".ccgp-stage-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("staged files left behind: %v, %v", stages, err)
	}
	if got := read(t, filepath.Join(dst, "agents", "old.md")); got != "old" {
		t.Fatalf("stale file changed: %q", got)
	}
	after, err := os.ReadFile(manifest)
	if err != nil || string(after) != string(before) {
		t.Fatalf("manifest changed: %v", err)
	}
	if err := os.Remove(filepath.Join(src, "plugins")); err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "agents", "old.md")); !os.IsNotExist(err) {
		t.Fatalf("stale file not pruned on retry: %v", err)
	}
}

func TestLegacyManifestUnchangedOnPreparationFailure(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	old := filepath.Join(dst, ".ccgp-mirror.json")
	write(t, old, `{"agents/old.md":{"size":1,"mtime":1}}`)
	write(t, filepath.Join(src, "plugins"), "not a directory")
	if err := PrepareOwn(src, dst); err == nil {
		t.Fatal("accepted invalid source")
	}
	if got := read(t, old); got != `{"agents/old.md":{"size":1,"mtime":1}}` {
		t.Fatalf("legacy manifest changed: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dst, manifestFile)); !os.IsNotExist(err) {
		t.Fatalf("new manifest published: %v", err)
	}
}

func TestPrunesTrackedTopLevelFiles(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	for _, name := range []string{"CLAUDE.md", "settings.json"} {
		content := "instructions"
		if name == "settings.json" {
			content = `{}`
		}
		write(t, filepath.Join(src, name), content)
	}
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE.md", "settings.json"} {
		if err := os.Remove(filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE.md", "settings.json"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); !os.IsNotExist(err) {
			t.Errorf("tracked %s remains: %v", name, err)
		}
	}
}

func TestPrunePreservesEmptyLocalDirectories(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	file := filepath.Join(src, "agents", "old.md")
	write(t, file, "old")
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dst, "agents", "local", "empty")
	if err := os.MkdirAll(local, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(local); err != nil || !st.IsDir() {
		t.Fatalf("local directory lost: %v", err)
	}
}

func TestRejectsUnsafeManifestPath(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "victim")
	write(t, victim, "safe")
	if err := (mirrored{"../victim": {}}).save(dst); err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwn(src, dst); err == nil {
		t.Fatal("accepted unsafe manifest")
	}
	if got := read(t, victim); got != "safe" {
		t.Fatalf("victim changed: %q", got)
	}
}
