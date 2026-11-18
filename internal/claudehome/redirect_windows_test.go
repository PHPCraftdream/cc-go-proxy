package claudehome

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func TestMirrorWithWindowsShortAncestor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "LongMirrorAncestorDirectory")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	name, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, 32768)
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetShortPathNameW")
	n, _, callErr := proc.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || n >= uintptr(len(buf)) {
		t.Skipf("short names unavailable: %v", callErr)
	}
	short := syscall.UTF16ToString(buf[:n])
	resolved, err := filepath.EvalSymlinks(short)
	if err != nil {
		t.Fatal(err)
	}
	if strings.EqualFold(filepath.Clean(short), filepath.Clean(resolved)) {
		t.Skip("filesystem did not provide a distinct short path")
	}
	src, dst := filepath.Join(root, "source"), filepath.Join(short, "profile")
	write(t, filepath.Join(src, "agents", "old.md"), "source")
	write(t, filepath.Join(dst, "agents", "old.md"), "old")
	write(t, filepath.Join(dst, ".ccgp-mirror.json"), `{"agents/old.md":{"size":3,"mtime":1}}`)
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dst, ".ccgp-mirror.json")) || !exists(filepath.Join(dst, manifestFile)) {
		t.Fatal("legacy manifest was not migrated")
	}
	if err := os.Remove(filepath.Join(src, "agents", "old.md")); err != nil {
		t.Fatal(err)
	}
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "agents", "old.md")); !os.IsNotExist(err) {
		t.Fatalf("stale file not removed: %v", err)
	}
}

func TestRecoveryRefusesWindowsJunctionAncestor(t *testing.T) {
	root := t.TempDir()
	dst, outside := filepath.Join(root, "profile"), filepath.Join(root, "outside")
	write(t, filepath.Join(outside, "file.md"), "safe")
	if err := os.MkdirAll(filepath.Join(dst, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	createTestJunction(t, filepath.Join(dst, "agents", "linked"), outside)
	if err := safeRecoveryPath(dst, "agents/linked/file.md"); err == nil {
		t.Fatal("accepted junction ancestor")
	}
	if got := read(t, filepath.Join(outside, "file.md")); got != "safe" {
		t.Fatalf("outside file changed: %q", got)
	}
}
