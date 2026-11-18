package claudehome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateManifestPaths(t *testing.T) {
	for _, key := range []string{"CLAUDE.md", "keybindings.json", "settings.json", "agents/a.md", "skills/s/SKILL.md", "plugins/cache/p/plugin.json", "output-styles/x.md"} {
		if err := validateManifest(mirrored{key: {}}); err != nil {
			t.Errorf("%q refused: %v", key, err)
		}
	}
	for _, key := range []string{
		"", ".", "..", "../x", "/abs/x", "agents/../x", "agents//x", "agents/./x", `agents\x`,
		"agents", "agents/", "other.md", "CLAUDE.md/x", "settings.json/x", "history.jsonl", "projects/x.jsonl",
		"agents/.credentials.json", "agents/.CREDENTIALS.JSON", "skills/s/.credentials.json", ".credentials.json",
	} {
		if err := validateManifest(mirrored{key: {}}); err == nil {
			t.Errorf("%q accepted", key)
		}
	}
}

func TestSafeRecoveryPath(t *testing.T) {
	dst := t.TempDir()
	write(t, filepath.Join(dst, "agents", "a.md"), "a")
	if err := os.MkdirAll(filepath.Join(dst, "skills", "dir.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dst, "commands"), "a file where a directory belongs")

	for key, ok := range map[string]bool{
		"agents/a.md":        true,  // a regular file
		"agents/new.md":      true,  // not there yet
		"plugins/x/y/z.json": true,  // no component there yet
		"skills/dir.md":      false, // a directory is not a managed file
		"commands/c.md":      false, // a file in the place of a directory
		"agents/a.md/deeper": false, // below a regular file
		"keybindings.json":   true,
	} {
		if err := safeRecoveryPath(dst, key); (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", key, err, ok)
		}
	}
}

func TestSafeRecoveryPathRefusesALinkedComponent(t *testing.T) {
	dst := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	write(t, filepath.Join(outside, "a.md"), "outside")
	if err := os.Symlink(outside, filepath.Join(dst, "agents")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := safeRecoveryPath(dst, "agents/a.md"); err == nil {
		t.Fatal("a path through a link was accepted")
	}
	if err := pruneStale(dst, mirrored{"agents/a.md": {}}, map[string]bool{}); err == nil {
		t.Fatal("pruning went through a link")
	}
	if got := read(t, filepath.Join(outside, "a.md")); got != "outside" {
		t.Fatalf("a file outside the destination changed: %q", got)
	}
}

func TestPruneStaleRemovesOnlyTrackedUnseenFiles(t *testing.T) {
	dst := t.TempDir()
	write(t, filepath.Join(dst, "agents", "kept.md"), "kept")
	write(t, filepath.Join(dst, "agents", "stale.md"), "stale")
	write(t, filepath.Join(dst, "agents", "local.md"), "made by the session")
	man := mirrored{"agents/kept.md": {}, "agents/stale.md": {}, "agents/already-gone.md": {}}
	if err := pruneStale(dst, man, map[string]bool{"agents/kept.md": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "agents", "stale.md")); !os.IsNotExist(err) {
		t.Errorf("a stale file survived: %v", err)
	}
	for _, name := range []string{"kept.md", "local.md"} {
		if _, err := os.Stat(filepath.Join(dst, "agents", name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
	if _, ok := man["agents/stale.md"]; ok || len(man) != 1 {
		t.Errorf("manifest after pruning: %v", man)
	}
}

// Every stale path is checked before the first one is removed: one unsafe entry
// leaves the rest, and the manifest, as they were.
func TestPruneStaleChecksEverythingBeforeRemoving(t *testing.T) {
	dst := t.TempDir()
	write(t, filepath.Join(dst, "agents", "a.md"), "a")
	if err := os.MkdirAll(filepath.Join(dst, "agents", "b.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	man := mirrored{"agents/a.md": {}, "agents/b.md": {}}
	err := pruneStale(dst, man, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "unsafe managed path") {
		t.Fatalf("err = %v", err)
	}
	if got := read(t, filepath.Join(dst, "agents", "a.md")); got != "a" {
		t.Errorf("a safe stale file was removed before the check failed: %q", got)
	}
	if len(man) != 2 {
		t.Errorf("manifest changed: %v", man)
	}
}
