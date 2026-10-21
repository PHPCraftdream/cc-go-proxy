package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The test binary doubles as a fake ccgp: copied to <dir>/ccgp, it prints its
// arguments and exits with the code in FAKE_CCGP_EXIT.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_CCGP") == "1" {
		_, _ = os.Stdout.WriteString(strings.Join(os.Args[1:], "|") + "\n")
		code := 0
		if os.Getenv("FAKE_CCGP_EXIT") == "7" {
			code = 7
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func TestCCGPArgsPutTheFlagAfterTheProfile(t *testing.T) {
	got, err := ccgpArgs([]string{"codex", "-c", "fix the bug"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "--dangerously-skip-permissions", "-c", "fix the bug"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("ccgpArgs = %q, want %q", got, want)
	}

	got, err = ccgpArgs([]string{"zai"})
	if err != nil || len(got) != 2 || got[0] != "zai" || got[1] != "--dangerously-skip-permissions" {
		t.Errorf("ccgpArgs(zai) = %q, %v", got, err)
	}
}

func TestCCGPArgsRejectsMissingProfile(t *testing.T) {
	for _, in := range [][]string{nil, {""}, {"--model", "x"}, {"-c"}} {
		if _, err := ccgpArgs(in); err == nil || !strings.Contains(err.Error(), "usage: ccgpy <profile>") {
			t.Errorf("ccgpArgs(%q) = %v, want the usage error", in, err)
		}
	}
}

func TestFindCCGPPrefersTheSibling(t *testing.T) {
	dir := t.TempDir()
	name := "ccgp"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	sibling := filepath.Join(dir, name)
	if err := os.WriteFile(sibling, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	failing := func(string) (string, error) { return "", errors.New("PATH must not be consulted") }
	got, err := findCCGP(dir, failing)
	if err != nil || got != sibling {
		t.Errorf("findCCGP = %q, %v; want %q", got, err, sibling)
	}
}

func TestFindCCGPFallsBackToPathThenFails(t *testing.T) {
	empty := t.TempDir()
	got, err := findCCGP(empty, func(n string) (string, error) { return "/usr/bin/" + n, nil })
	if err != nil || got != "/usr/bin/ccgp" {
		t.Errorf("PATH fallback = %q, %v", got, err)
	}
	if _, err := findCCGP(empty, func(string) (string, error) { return "", errors.New("nope") }); err == nil ||
		!strings.Contains(err.Error(), "ccgp not found") {
		t.Errorf("missing ccgp: %v", err)
	}
}

// fakeCCGP copies the test binary to <dir>/ccgp and returns dir.
func fakeCCGP(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "ccgp"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CCGP", "1")
	return dir
}

func TestRunPassesArgumentsAndExitCodeThrough(t *testing.T) {
	dir := fakeCCGP(t)

	var out, errOut bytes.Buffer
	if code := run([]string{"codex", "-c"}, dir, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "codex|--dangerously-skip-permissions|-c" {
		t.Errorf("ccgp was started with %q", got)
	}

	t.Setenv("FAKE_CCGP_EXIT", "7")
	out.Reset()
	if code := run([]string{"zai"}, dir, &out, &errOut); code != 7 {
		t.Errorf("exit code = %d, want 7 passed through", code)
	}
}

func TestRunUsageAndMissingCCGP(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"help"}, "", &out, &errOut); code != 0 || !strings.Contains(out.String(), "usage: ccgpy <profile>") {
		t.Errorf("help: exit %d, out %q", code, out.String())
	}
	if code := run(nil, "", &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "usage:") {
		t.Errorf("no args: exit %d, stderr %q", code, errOut.String())
	}

	// No sibling and an empty PATH: ccgp cannot be found.
	t.Setenv("PATH", t.TempDir())
	errOut.Reset()
	if code := run([]string{"codex"}, t.TempDir(), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "ccgp not found") {
		t.Errorf("missing ccgp: exit %d, stderr %q", code, errOut.String())
	}
}
