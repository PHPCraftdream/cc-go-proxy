// Command ccgpy is the no-prompts shortcut of ccgp, like cy for claude:
//
//	ccgpy <profile> [claude args...]
//
// runs
//
//	ccgp <profile> --dangerously-skip-permissions [claude args...]
//
// It starts the ccgp binary that sits next to it (or the one in PATH), passes
// the terminal and the exit code through, and does nothing else.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
)

// skipPermissions is Claude Code's flag that disables every permission prompt.
const skipPermissions = "--dangerously-skip-permissions"

const usage = "usage: ccgpy <profile> [claude args...]   (runs: ccgp <profile> " + skipPermissions + " [claude args...])"

func main() {
	self, _ := os.Executable()
	os.Exit(run(os.Args[1:], filepath.Dir(self), os.Stdout, os.Stderr))
}

// run launches ccgp for the profile in args[0] and returns the exit code.
// selfDir is where a sibling ccgp binary is looked for before PATH.
func run(args []string, selfDir string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		_, _ = fmt.Fprintln(stdout, usage)
		return 0
	}
	argv, err := ccgpArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ccgpy: %v\n", err)
		return 2
	}
	ccgp, err := findCCGP(selfDir, exec.LookPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ccgpy: %v\n", err)
		return 1
	}

	cmd := exec.Command(ccgp, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr

	// Ctrl+C reaches ccgp and claude through the terminal; this wrapper must
	// outlive them to report the exit code.
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		_, _ = fmt.Fprintf(stderr, "ccgpy: %v\n", err)
		return 1
	}
	return 0
}

// ccgpArgs puts the permission-skipping flag right after the profile.
func ccgpArgs(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, errors.New(usage)
	}
	if args[0] == "" || args[0][0] == '-' {
		return nil, fmt.Errorf("the first argument must be a profile, got %q\n%s", args[0], usage)
	}
	out := make([]string, 0, len(args)+1)
	out = append(out, args[0], skipPermissions)
	return append(out, args[1:]...), nil
}

// findCCGP prefers the ccgp next to this binary, so a release archive or an
// npm package works without PATH, and falls back to PATH.
func findCCGP(selfDir string, lookPath func(string) (string, error)) (string, error) {
	name := "ccgp"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if selfDir != "" {
		p := filepath.Join(selfDir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	p, err := lookPath("ccgp")
	if err != nil {
		return "", errors.New("ccgp not found next to ccgpy or in PATH")
	}
	return p, nil
}
