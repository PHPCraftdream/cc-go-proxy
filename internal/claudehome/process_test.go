//go:build windows || linux || darwin || freebsd || openbsd || netbsd || dragonfly

package claudehome

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPrepareProcessHelper runs only in a child test process.
func TestPrepareProcessHelper(t *testing.T) {
	if os.Getenv("CCGP_PREPARE_HELPER") != "1" {
		return
	}
	if os.Getenv("CCGP_PREPARE_PROBE") == "1" {
		acquireDestinationLock = func(f *os.File) error {
			if err := expectContendedLock(f); err != nil {
				return err
			}
			fmt.Println("contended")
			return lockFile(f)
		}
	} else {
		fmt.Println("ready")
	}
	src, dst := os.Getenv("CCGP_PREPARE_SOURCE"), os.Getenv("CCGP_PREPARE_DEST")
	var err error
	if os.Getenv("CCGP_PREPARE_MODE") == "proxy" {
		err = Prepare(src, filepath.Join(src, ".claude.json"), dst)
	} else {
		err = PrepareOwn(src, dst)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type prepareChild struct {
	done   chan error
	stderr bytes.Buffer
}

func startPrepareChild(t *testing.T, src, dst, mode string, probe bool) *prepareChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPrepareProcessHelper$")
	cmd.Env = append(os.Environ(), "CCGP_PREPARE_HELPER=1", "CCGP_PREPARE_SOURCE="+src, "CCGP_PREPARE_DEST="+dst, "CCGP_PREPARE_MODE="+mode)
	marker := "ready"
	if probe {
		cmd.Env = append(cmd.Env, "CCGP_PREPARE_PROBE=1")
		marker = "contended"
	}
	child := &prepareChild{done: make(chan error, 1)}
	cmd.Stderr = &child.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() || scanner.Text() != marker {
			ready <- fmt.Errorf("helper did not signal ready: %v", scanner.Err())
		} else {
			ready <- nil
		}
		// Drain the test runner's output before Wait closes the pipe.
		for scanner.Scan() {
		}
		child.done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		cancel()
		// Wait is called by the reader goroutine even if the test fails.
		<-child.done
	})
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("helper did not become ready before deadline")
	}
	return child
}

func waitPrepareChild(t *testing.T, child *prepareChild, wantError bool) {
	t.Helper()
	select {
	case err := <-child.done:
		// Leave the result for cleanup, which also guarantees the child was reaped.
		child.done <- err
		if wantError {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(child.stderr.String(), "invalid JSON") {
				t.Fatalf("expected invalid-settings error, got %v, stderr = %s", err, child.stderr.String())
			}
		} else if err != nil {
			t.Fatalf("helper error = %v, stderr = %s", err, child.stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("preparation exceeded deadline")
	}
}

func TestPrepareProcessesLockPerDestination(t *testing.T) {
	for _, mode := range []string{"proxy", "own"} {
		t.Run(mode, func(t *testing.T) { testPrepareProcessesLockPerDestination(t, mode) })
	}
}

func testPrepareProcessesLockPerDestination(t *testing.T, mode string) {
	t.Helper()
	root := t.TempDir()
	src, first, second := filepath.Join(root, "src"), filepath.Join(root, "first"), filepath.Join(root, "second")
	write(t, filepath.Join(src, "CLAUDE.md"), "shared")
	unlock, err := lockDestination(src, first)
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	same := startPrepareChild(t, src, first, mode, true)
	other := startPrepareChild(t, src, second, "own", false)
	waitPrepareChild(t, other, false)
	unlock()
	locked = false
	waitPrepareChild(t, same, false)
	for _, dst := range []string{first, second} {
		if got := read(t, filepath.Join(dst, "CLAUDE.md")); got != "shared" {
			t.Fatalf("%s: got %q", dst, got)
		}
	}
}

func TestPrepareProcessRetryPreservesLocalData(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "CLAUDE.md"), "source")
	write(t, filepath.Join(src, "settings.json"), "invalid")
	write(t, filepath.Join(src, credentialsFile), "source login")
	write(t, filepath.Join(dst, credentialsFile), "own login")
	write(t, filepath.Join(dst, "projects", "session.jsonl"), "local session")
	waitPrepareChild(t, startPrepareChild(t, src, dst, "own", false), true)
	write(t, filepath.Join(src, "settings.json"), `{}`)
	waitPrepareChild(t, startPrepareChild(t, src, dst, "own", false), false)
	for path, want := range map[string]string{
		filepath.Join(src, "CLAUDE.md"):                 "source",
		filepath.Join(src, credentialsFile):             "source login",
		filepath.Join(dst, credentialsFile):             "own login",
		filepath.Join(dst, "projects", "session.jsonl"): "local session",
		filepath.Join(dst, "CLAUDE.md"):                 "source",
	} {
		if got := read(t, path); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}
