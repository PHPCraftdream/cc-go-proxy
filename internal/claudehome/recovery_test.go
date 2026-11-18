package claudehome

import (
	"bytes"
	"os"
	"path/filepath"
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
	var staged []*stagedFile
	for _, item := range []struct{ name, value string }{
		{"agents/a.md", "new a"},
		{"agents/b.md", "new b"},
		{"plugins/installed.json", `{"updated":true}`},
	} {
		file, err := stageFile(filepath.Join(dst, filepath.FromSlash(item.name)), bytes.NewBufferString(item.value), nil)
		if err != nil {
			t.Fatal(err)
		}
		staged = append(staged, file)
		defer file.discard()
	}
	if err := recordRecovery(dst, recoveryState{}, staged, previous); err != nil {
		t.Fatal(err)
	}
	if err := staged[0].publish(false); err != nil {
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
