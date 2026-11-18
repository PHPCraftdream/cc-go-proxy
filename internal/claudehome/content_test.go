package claudehome

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	if err := publishFile(path, r, nil, false); err == nil {
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
