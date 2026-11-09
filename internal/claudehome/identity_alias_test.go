package claudehome

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const ownConfigDirError = "destination is the user's own Claude Code config dir"

type aliasedConfigEntry struct {
	isDir bool
	data  []byte
}

type configDirAlias struct {
	name string
	kind string
}

func TestPrepareRejectsAliasedOwnConfigDir(t *testing.T) {
	testAliasedOwnConfigDirRefusal(t, func(src, srcState, dst string) error {
		return Prepare(src, srcState, dst)
	})
}

func TestPrepareOwnRejectsAliasedOwnConfigDir(t *testing.T) {
	testAliasedOwnConfigDirRefusal(t, func(src, _ string, dst string) error {
		return PrepareOwn(src, dst)
	})
}

func testAliasedOwnConfigDirRefusal(t *testing.T, prepare func(src, srcState, dst string) error) {
	t.Helper()

	aliases := []configDirAlias{
		{name: "destination symlink", kind: "symlink"},
		{name: "source symlink", kind: "symlink"},
	}
	if runtime.GOOS == "windows" {
		aliases = append(aliases,
			configDirAlias{name: "destination junction", kind: "junction"},
			configDirAlias{name: "source junction", kind: "junction"},
		)
	}

	for _, alias := range aliases {
		t.Run(alias.name, func(t *testing.T) {
			root := t.TempDir()
			realSrc := filepath.Join(root, "real-src")
			if err := os.MkdirAll(realSrc, 0o700); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(realSrc, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"must-stay"}}`)
			write(t, filepath.Join(realSrc, "settings.json"), `{"apiKeyHelper":"must-stay","env":{"ANTHROPIC_API_KEY":"must-stay"}}`)
			write(t, filepath.Join(realSrc, ".claude.json"), `{"userID":"must-stay","oauthAccount":{"emailAddress":"must-stay@example.com"}}`)
			write(t, filepath.Join(realSrc, "CLAUDE.md"), "must-stay instructions")

			aliasPath := filepath.Join(root, "alias")
			if alias.kind == "junction" {
				createTestJunction(t, aliasPath, realSrc)
			} else {
				if err := os.Symlink(realSrc, aliasPath); err != nil {
					t.Skipf("directory symlink unavailable: %v", err)
				}
			}

			src, dst := realSrc, aliasPath
			if strings.HasPrefix(alias.name, "source ") {
				src, dst = aliasPath, realSrc
			}
			srcState := filepath.Join(src, ".claude.json")
			before := snapshotConfigDir(t, realSrc)

			err := prepare(src, srcState, dst)

			assertConfigDirUnchanged(t, before, snapshotConfigDir(t, realSrc))
			if err == nil {
				t.Errorf("aliased own config dir was accepted; want error containing %q", ownConfigDirError)
			} else if !strings.Contains(err.Error(), ownConfigDirError) {
				t.Errorf("error = %q, want it to contain %q", err, ownConfigDirError)
			}
		})
	}
}

func createTestJunction(t *testing.T, link, target string) {
	t.Helper()
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("directory junction unavailable: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

func snapshotConfigDir(t *testing.T, root string) map[string]aliasedConfigEntry {
	t.Helper()
	entries := make(map[string]aliasedConfigEntry)
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entry := aliasedConfigEntry{isDir: d.IsDir()}
		if !d.IsDir() {
			entry.data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		entries[rel] = entry
		return nil
	}); err != nil {
		t.Fatalf("snapshot config dir %q: %v", root, err)
	}
	return entries
}

func assertConfigDirUnchanged(t *testing.T, before, after map[string]aliasedConfigEntry) {
	t.Helper()
	var names []string
	for name := range before {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := before[name]
		got, ok := after[name]
		if !ok {
			t.Errorf("source entry %q was removed", name)
			continue
		}
		if want.isDir != got.isDir || !bytes.Equal(want.data, got.data) {
			t.Errorf("source entry %q changed: before=%q after=%q", name, want.data, got.data)
		}
	}
	names = names[:0]
	for name := range after {
		if _, ok := before[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		t.Errorf("unexpected source entry created: %q", name)
	}
}
