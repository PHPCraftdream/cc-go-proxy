package claudehome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func prepare(t *testing.T, src, dst string) {
	t.Helper()
	if err := Prepare(src, filepath.Join(filepath.Dir(src), "none.json"), dst); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorCopiesUpdatesAndPrunes(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "agents", "a.md"), "a1")
	write(t, filepath.Join(src, "agents", "b.md"), "b1")
	write(t, filepath.Join(src, "commands", "sub", "c.md"), "c1")
	prepare(t, src, dst)
	for p, want := range map[string]string{"agents/a.md": "a1", "agents/b.md": "b1", "commands/sub/c.md": "c1"} {
		if got := read(t, filepath.Join(dst, filepath.FromSlash(p))); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}

	// The session creates its own agent; the source changes and drops files.
	write(t, filepath.Join(dst, "agents", "own.md"), "mine")
	write(t, filepath.Join(src, "agents", "a.md"), "a2-longer")
	if err := os.Remove(filepath.Join(src, "agents", "b.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(src, "commands")); err != nil {
		t.Fatal(err)
	}
	prepare(t, src, dst)

	if got := read(t, filepath.Join(dst, "agents", "a.md")); got != "a2-longer" {
		t.Errorf("a.md not updated: %q", got)
	}
	if exists(filepath.Join(dst, "agents", "b.md")) {
		t.Error("b.md dropped at the source is still mirrored")
	}
	if exists(filepath.Join(dst, "commands", "sub", "c.md")) || exists(filepath.Join(dst, "commands", "sub")) {
		t.Error("commands removed at the source are still mirrored")
	}
	if read(t, filepath.Join(dst, "agents", "own.md")) != "mine" {
		t.Error("a file created in the isolated dir was removed")
	}
	// The source is only ever read.
	if read(t, filepath.Join(src, "agents", "a.md")) != "a2-longer" {
		t.Error("source modified")
	}
}

func TestMirrorSkipsCredentialsEverywhere(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "plugins", "x", ".credentials.json"), "official")
	write(t, filepath.Join(src, "skills", "s", ".Credentials.json"), "official")
	write(t, filepath.Join(src, "skills", "s", "SKILL.md"), "ok")
	write(t, filepath.Join(src, ".credentials.json"), "official")
	prepare(t, src, dst)

	var found []string
	_ = filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && isCredential(d.Name()) {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Fatalf("credential files copied: %v", found)
	}
	if read(t, filepath.Join(dst, "skills", "s", "SKILL.md")) != "ok" {
		t.Error("regular files next to a credential file were not copied")
	}
}

func TestMirrorReplacesLinkInsteadOfWritingThroughIt(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "skills", "s1", "SKILL.md"), "official")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	// A leftover link from an older layout: dst/skills -> src/skills.
	if err := os.Symlink(filepath.Join(src, "skills"), filepath.Join(dst, "skills")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	write(t, filepath.Join(src, "skills", "s2", "SKILL.md"), "new")
	prepare(t, src, dst)

	st, err := os.Lstat(filepath.Join(dst, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		t.Fatal("dst/skills is still a link")
	}
	if read(t, filepath.Join(dst, "skills", "s2", "SKILL.md")) != "new" {
		t.Error("content not copied after replacing the link")
	}
	if read(t, filepath.Join(src, "skills", "s1", "SKILL.md")) != "official" {
		t.Error("source changed")
	}
	// Editing the copy must not reach the source.
	write(t, filepath.Join(dst, "skills", "s1", "SKILL.md"), "edited in session")
	if read(t, filepath.Join(src, "skills", "s1", "SKILL.md")) != "official" {
		t.Error("write to the copy reached the source")
	}
}

func TestMirrorRewritesPluginPaths(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	srcPlug, dstPlug := filepath.Join(src, "plugins"), filepath.Join(dst, "plugins")
	body := `{"installPath": "` + jsonEscape(filepath.Join(srcPlug, "cache", "p")) + `",` +
		` "url": "` + filepath.ToSlash(filepath.Join(srcPlug, "marketplaces", "m")) + `",` +
		` "external": "` + jsonEscape(filepath.Join(root, "elsewhere", "m")) + `"}`
	write(t, filepath.Join(srcPlug, "installed_plugins.json"), body)
	write(t, filepath.Join(srcPlug, "cache", "p", "plugin.json"), "{}")
	prepare(t, src, dst)

	got := read(t, filepath.Join(dstPlug, "installed_plugins.json"))
	if strings.Contains(got, jsonEscape(srcPlug)) || strings.Contains(got, filepath.ToSlash(srcPlug)) {
		t.Errorf("source plugin path survived: %s", got)
	}
	if !strings.Contains(got, jsonEscape(filepath.Join(dstPlug, "cache", "p"))) ||
		!strings.Contains(got, filepath.ToSlash(filepath.Join(dstPlug, "marketplaces", "m"))) {
		t.Errorf("paths not pointed at the isolated copy: %s", got)
	}
	if !strings.Contains(got, jsonEscape(filepath.Join(root, "elsewhere", "m"))) {
		t.Errorf("unrelated path rewritten: %s", got)
	}
	if read(t, filepath.Join(srcPlug, "installed_plugins.json")) != body {
		t.Error("source metadata modified")
	}

	// Session changes to the metadata survive while the source is unchanged,
	// and a second launch is a no-op.
	mod := strings.Replace(got, "{", `{"added": true, `, 1)
	write(t, filepath.Join(dstPlug, "installed_plugins.json"), mod)
	prepare(t, src, dst)
	if read(t, filepath.Join(dstPlug, "installed_plugins.json")) != mod {
		t.Error("session edit of mirrored metadata was overwritten without a source change")
	}
}

func TestMirrorUnchangedFilesAreNotRecopied(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "agents", "a.md"), "same")
	prepare(t, src, dst)
	target := filepath.Join(dst, "agents", "a.md")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatal(err)
	}
	prepare(t, src, dst)
	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(old) {
		t.Error("unchanged file was copied again")
	}
}

func TestManifestIsKtavAndKeepsOddPaths(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "agents", "a.b.md"), "dotted")
	write(t, filepath.Join(src, "commands", "my cmd #1.md"), "spaced")
	write(t, filepath.Join(src, "skills", "ключ", "SKILL.md"), "unicode")
	prepare(t, src, dst)

	if !exists(filepath.Join(dst, ".ccgp-mirror.ktav")) || exists(filepath.Join(dst, ".ccgp-mirror.json")) {
		t.Fatal("the manifest is not a .ktav file")
	}
	m := loadManifest(dst)
	for _, p := range []string{"agents/a.b.md", "commands/my cmd #1.md", "skills/ключ/SKILL.md"} {
		if _, ok := m[p]; !ok {
			t.Errorf("%q missing from the manifest: %v", p, m)
		}
	}
	// A second run finds the manifest and copies nothing again.
	before := read(t, filepath.Join(dst, ".ccgp-mirror.ktav"))
	prepare(t, src, dst)
	if after := read(t, filepath.Join(dst, ".ccgp-mirror.ktav")); after != before {
		t.Error("the manifest changed without a change in the source")
	}
}

func TestLegacyJSONManifestIsMigrated(t *testing.T) {
	dst := t.TempDir()
	old := filepath.Join(dst, ".ccgp-mirror.json")
	write(t, old, `{"agents/a.md": {"size": 2, "mtime": 1700000000123456789}, "commands/x.y.md": {"size": 5, "mtime": 42}}`)

	m := loadManifest(dst)
	if len(m) != 2 || m["agents/a.md"] != (stamp{Size: 2, ModTime: 1700000000123456789}) || m["commands/x.y.md"] != (stamp{Size: 5, ModTime: 42}) {
		t.Fatalf("migrated manifest = %v", m)
	}
	if exists(old) || !exists(filepath.Join(dst, ".ccgp-mirror.ktav")) {
		t.Error("legacy file kept or Ktav file missing")
	}
	if m2 := loadManifest(dst); len(m2) != 2 || m2["commands/x.y.md"].Size != 5 {
		t.Errorf("manifest after migration = %v", m2)
	}
}

func TestDamagedManifestCostsOnlyARecopy(t *testing.T) {
	dst := t.TempDir()
	write(t, filepath.Join(dst, ".ccgp-mirror.ktav"), "files: [\n")
	if m := loadManifest(dst); len(m) != 0 {
		t.Errorf("damaged manifest read as %v", m)
	}
}
