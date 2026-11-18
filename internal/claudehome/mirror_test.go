package claudehome

import (
	"crypto/sha256"
	"fmt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/claudehome/safefs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func loadManifest(dst string) mirrored {
	m, _, _ := loadManifestSync(dst)
	return m
}

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
	if exists(filepath.Join(dst, "commands", "sub", "c.md")) {
		t.Error("command removed at the source is still mirrored")
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

func TestMirrorRefusesLinkInsteadOfWritingThroughIt(t *testing.T) {
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
	if err := PrepareOwn(src, dst); err == nil {
		t.Fatal("accepted destination link")
	}
	st, err := os.Lstat(filepath.Join(dst, "skills"))
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destination link changed: %v", err)
	}
	if read(t, filepath.Join(src, "skills", "s1", "SKILL.md")) != "official" {
		t.Error("source changed")
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
	if !exists(old) || exists(filepath.Join(dst, ".ccgp-mirror.ktav")) {
		t.Fatal("reading the legacy manifest changed it")
	}
	src := filepath.Join(t.TempDir(), "src")
	write(t, filepath.Join(src, "agents", "a.md"), "ab")
	write(t, filepath.Join(src, "commands", "x.y.md"), "12345")
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
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

func TestLegacyStampPreservesLocalEdit(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	path := filepath.Join(src, "agents", "a.md")
	write(t, path, "source")
	prepare(t, src, dst)
	write(t, filepath.Join(dst, "agents", "a.md"), "local")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dst, manifestFile)); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dst, ".ccgp-mirror.json"), fmt.Sprintf(`{"agents/a.md":{"size":%d,"mtime":%d}}`, info.Size(), info.ModTime().UnixNano()))
	prepare(t, src, dst)
	if got := read(t, filepath.Join(dst, "agents", "a.md")); got != "local" {
		t.Fatalf("local edit = %q", got)
	}
	if loadManifest(dst)["agents/a.md"].Hash == "" {
		t.Fatal("legacy stamp not upgraded")
	}
}

func TestDigestMismatchDoesNotPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target")
	write(t, path, "original")
	r := &digestReader{reader: strings.NewReader("changed"), hash: sha256.New(), expected: "wrong"}
	if err := safefs.PublishFile(path, r, nil, false); err == nil {
		t.Fatal("unstable source accepted")
	}
	if got := read(t, path); got != "original" {
		t.Fatalf("published %q", got)
	}
}

func TestMirrorContentIdentity(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(map[bool]string{false: "proxy", true: "own"}[own], func(t *testing.T) {
			root := t.TempDir()
			src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
			source := filepath.Join(src, "agents", "a.md")
			target := filepath.Join(dst, "agents", "a.md")
			write(t, source, "one")
			run := func() {
				t.Helper()
				var err error
				if own {
					err = PrepareOwn(src, dst)
				} else {
					err = Prepare(src, filepath.Join(root, "state"), dst)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			run()
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			touch := time.Unix(1700000000, 0)
			if err := os.Chtimes(source, touch, touch); err != nil {
				t.Fatal(err)
			}
			run()
			after, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("touch replaced identical destination")
			}
			write(t, target, "local")
			run()
			if read(t, target) != "local" {
				t.Fatal("local edit lost")
			}
			write(t, source, "two")
			if err := os.Chtimes(source, touch, touch); err != nil {
				t.Fatal(err)
			}
			run()
			if read(t, target) != "two" {
				t.Fatal("same-stamp source change missed")
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			run()
			if read(t, target) != "two" {
				t.Fatal("missing target not restored")
			}
		})
	}
}

func TestMirrorTransformedContentAndManifestRecovery(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	plugin := filepath.Join(src, "plugins", "installed.json")
	write(t, plugin, `{"path":"`+filepath.ToSlash(filepath.Join(src, "plugins", "x"))+`"}`)
	write(t, filepath.Join(src, "settings.json"), `{"model":"x","apiKeyHelper":"secret"}`)
	prepare(t, src, dst)
	target := filepath.Join(dst, "plugins", "installed.json")
	if strings.Contains(read(t, target), filepath.ToSlash(filepath.Join(src, "plugins"))) {
		t.Fatal("source path published")
	}
	for _, manifest := range []string{"missing", "damaged", "legacy"} {
		t.Run(manifest, func(t *testing.T) {
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			m := filepath.Join(dst, manifestFile)
			if err := os.Remove(m); err != nil {
				t.Fatal(err)
			}
			switch manifest {
			case "damaged":
				write(t, m, "files: [\n")
			case "legacy":
				write(t, filepath.Join(dst, ".ccgp-mirror.json"), `{"plugins/installed.json":{"size":1,"mtime":1}}`)
			}
			prepare(t, src, dst)
			after, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("identical transformed file replaced")
			}
		})
	}
	settings := filepath.Join(dst, "settings.json")
	before, err := os.Stat(settings)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "settings.json"), `{"model":"x","apiKeyHelper":"other"}`)
	prepare(t, src, dst)
	after, err := os.Stat(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("identical sanitized settings replaced")
	}
}

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
