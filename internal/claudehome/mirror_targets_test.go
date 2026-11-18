package claudehome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A directory where a mirrored file belongs fails the whole preparation before
// anything is published, and leaves no staged file behind.
func TestMirrorRefusesANonFileTargetBeforePublishing(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "agents", "0-first.md"), "first")
	write(t, filepath.Join(src, "agents", "1-clash.md"), "clash")
	if err := os.MkdirAll(filepath.Join(dst, "agents", "1-clash.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := PrepareOwn(src, dst)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "agents", "0-first.md")); !os.IsNotExist(statErr) {
		t.Errorf("a file was published although the preparation failed: %v", statErr)
	}
	assertNoStages(t, filepath.Join(dst, "agents"))
	if st, statErr := os.Stat(filepath.Join(dst, "agents", "1-clash.md")); statErr != nil || !st.IsDir() {
		t.Errorf("the directory in the way was touched: %v", statErr)
	}
}

func TestMirrorRefusesANonFileTopLevelTarget(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "CLAUDE.md"), "instructions")
	write(t, filepath.Join(src, "keybindings.json"), "{}")
	if err := os.MkdirAll(filepath.Join(dst, "CLAUDE.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwn(src, dst); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "keybindings.json")); !os.IsNotExist(err) {
		t.Errorf("a file was published although the preparation failed: %v", err)
	}
}

// A settings file that cannot be read stops the preparation before the other
// files are published.
func TestMirrorStopsOnUnreadableSettings(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "CLAUDE.md"), "instructions")
	write(t, filepath.Join(src, "settings.json"), "{not json")
	err := PrepareOwn(src, dst)
	if err == nil || !strings.Contains(err.Error(), "settings.json") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "CLAUDE.md")); !os.IsNotExist(statErr) {
		t.Errorf("CLAUDE.md was published although settings.json failed: %v", statErr)
	}
	assertNoStages(t, dst)
}

// Only the JSON files directly in plugins/ carry the source path; nested files
// and other kinds are copied as they are.
func TestMirrorRewritesOnlyTopLevelPluginJSON(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	srcPlug := filepath.Join(src, "plugins")
	body := `{"path": "` + jsonEscape(filepath.Join(srcPlug, "cache")) + `"}`
	write(t, filepath.Join(srcPlug, "top.json"), body)
	write(t, filepath.Join(srcPlug, "cache", "nested.json"), body)
	write(t, filepath.Join(srcPlug, "notes.txt"), srcPlug)
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	dstPlug := filepath.Join(dst, "plugins")
	if got := read(t, filepath.Join(dstPlug, "top.json")); strings.Contains(got, jsonEscape(srcPlug)) || !strings.Contains(got, jsonEscape(dstPlug)) {
		t.Errorf("top-level metadata not rewritten: %s", got)
	}
	if got := read(t, filepath.Join(dstPlug, "cache", "nested.json")); got != body {
		t.Errorf("nested file changed: %s", got)
	}
	if got := read(t, filepath.Join(dstPlug, "notes.txt")); got != srcPlug {
		t.Errorf("non-JSON file changed: %s", got)
	}
}

// A source file that is gone, or turns into a link, is pruned from the copy
// only when an earlier run put it there.
func TestMirrorKeepsSessionFilesWhenSourceFileVanishes(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "agents", "tracked.md"), "tracked")
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dst, "agents", "session.md"), "made by the session")
	if err := os.Remove(filepath.Join(src, "agents", "tracked.md")); err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "agents", "tracked.md")); !os.IsNotExist(err) {
		t.Errorf("a file the source no longer has was kept: %v", err)
	}
	if got := read(t, filepath.Join(dst, "agents", "session.md")); got != "made by the session" {
		t.Errorf("a session file was removed: %q", got)
	}
}
