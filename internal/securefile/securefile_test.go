package securefile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteFileCreatesDirAndContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "secret.json")
	if err := WriteFile(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "one" {
		t.Fatalf("content = %q, %v", b, err)
	}
}

func TestWriteFileReplacesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")
	for _, v := range []string{"first", "second, longer"} {
		if err := WriteFile(path, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != "second, longer" {
		t.Errorf("content = %q", b)
	}
	// A shorter rewrite must not keep the tail of the longer one.
	if err := WriteFile(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "x" {
		t.Errorf("content after shorter write = %q", b)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestWriteFileOwnerOnlyOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses a DACL, not mode bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "secret.json")
	if err := WriteFile(path, []byte("s")); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}
	if st, err := os.Stat(filepath.Dir(path)); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, %v; want 0700", st.Mode().Perm(), err)
	}
}

func TestWriteFileFailsWhenDirIsAFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(filepath.Join(blocker, "secret.json"), []byte("s")); err == nil {
		t.Error("write under a regular file succeeded")
	}
}

func TestRestrictToOwnerKeepsFileUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("data"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := RestrictToOwner(path); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "data" {
		t.Errorf("owner cannot read its own file: %q, %v", b, err)
	}
}
