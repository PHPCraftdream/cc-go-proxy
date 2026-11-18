package claudehome

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDestinationLockStableAndIndependent(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	unlockFirst, err := lockDestination(src, first)
	if err != nil {
		t.Fatal(err)
	}
	unlockSecond, err := lockDestination(src, second)
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
	unlockAgain, err := lockDestination(src, first)
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
	if unlock, err := lockDestination(src, filepath.Join(src, "profile")); err == nil {
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
	if unlock, err := lockDestination(src, dst); err == nil {
		unlock()
		t.Fatal("accepted lock symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("source target changed: %q, %v", data, err)
	}
}
