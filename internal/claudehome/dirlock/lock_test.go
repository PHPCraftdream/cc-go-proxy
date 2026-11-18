package dirlock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDestinationLockStableAndIndependent(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	unlockFirst, err := Lock(src, first)
	if err != nil {
		t.Fatal(err)
	}
	unlockSecond, err := Lock(src, second)
	if err != nil {
		unlockFirst()
		t.Fatal(err)
	}
	unlockSecond()
	path := filepath.Join(first, destinationLockFile)
	before, err := os.Stat(path)
	if err != nil {
		unlockFirst()
		t.Fatal(err)
	}
	unlockFirst()
	unlockAgain, err := Lock(src, first)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockAgain()
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("lock object changed: %v", err)
	}
}

func TestDestinationLockRefusesSourceAndLockLink(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if unlock, err := Lock(src, filepath.Join(src, "profile")); err == nil {
		unlock()
		t.Fatal("accepted destination inside source")
	}
	if _, err := os.Stat(filepath.Join(src, "profile")); !os.IsNotExist(err) {
		t.Fatalf("created source path: %v", err)
	}
	dst := filepath.Join(root, "profile")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(src, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dst, destinationLockFile)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if unlock, err := Lock(src, dst); err == nil {
		unlock()
		t.Fatal("accepted lock symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("source target changed: %q, %v", data, err)
	}
}

// A second preparation of the same destination waits for the first to finish.
func TestDestinationLockWaitsForRelease(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "source"), filepath.Join(root, "profile")
	unlock, err := Lock(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() {
		next, err := Lock(src, dst)
		if err != nil {
			t.Error(err)
			next = func() {}
		}
		acquired <- next
	}()
	select {
	case next := <-acquired:
		next()
		unlock()
		t.Fatal("the lock was taken while another preparation held it")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case next := <-acquired:
		next()
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting preparation never got the lock")
	}
}

func TestDestinationLockRefusesANonFileLock(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "source"), filepath.Join(root, "profile")
	if err := os.MkdirAll(filepath.Join(dst, destinationLockFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if unlock, err := Lock(src, dst); err == nil {
		unlock()
		t.Fatal("a directory was accepted as the lock file")
	}
}

// A destination that is a file cannot be a profile directory.
func TestDestinationLockRefusesAFileDestination(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "profile")
	write(t, dst, "not a directory")
	if unlock, err := Lock(filepath.Join(root, "source"), dst); err == nil {
		unlock()
		t.Fatal("a file was accepted as the destination")
	}
	if got := read(t, dst); got != "not a directory" {
		t.Fatalf("destination changed: %q", got)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
