package claudehome

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A second preparation of the same destination waits for the first to finish.
func TestDestinationLockWaitsForRelease(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "source"), filepath.Join(root, "profile")
	unlock, err := lockDestination(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() {
		next, err := lockDestination(src, dst)
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
	if unlock, err := lockDestination(src, dst); err == nil {
		unlock()
		t.Fatal("a directory was accepted as the lock file")
	}
}

// A destination that is a file cannot be a profile directory.
func TestDestinationLockRefusesAFileDestination(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "profile")
	write(t, dst, "not a directory")
	if unlock, err := lockDestination(filepath.Join(root, "source"), dst); err == nil {
		unlock()
		t.Fatal("a file was accepted as the destination")
	}
	if got := read(t, dst); got != "not a directory" {
		t.Fatalf("destination changed: %q", got)
	}
}

// The lock is held only while preparing: a profile prepared twice in a row
// does not deadlock on its own lock.
func TestPrepareReleasesTheLock(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "agents", "a.md"), "a")
	for i := 0; i < 3; i++ {
		if err := PrepareOwn(src, dst); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	unlock, err := lockDestination(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
