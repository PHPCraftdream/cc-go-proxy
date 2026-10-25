package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

func TestCommandHelpTextHasEntryOrFallback(t *testing.T) {
	for _, c := range commands() {
		var buf bytes.Buffer
		printCommandHelp(&buf, c)
		out := buf.String()
		if out == "" {
			t.Errorf("%s: empty help text", c.name)
		}
		if !strings.Contains(out, c.name) {
			t.Errorf("%s: help text does not mention the command:\n%s", c.name, out)
		}
		if _, ok := commandHelp[c.name]; !ok && !strings.Contains(out, c.desc) {
			t.Errorf("%s: fallback help must contain the one-line desc, got:\n%s", c.name, out)
		}
	}
}

func TestCommandHelpTextFallback(t *testing.T) {
	c := command{name: "--restart", args: "", desc: "Restart the background server"}
	var buf bytes.Buffer
	printCommandHelp(&buf, c)
	want := "ccgp --restart\n\nRestart the background server\n"
	if buf.String() != want {
		t.Errorf("fallback help = %q, want %q", buf.String(), want)
	}
}

func TestWantsHelp(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"command with --help", []string{"--login", "--help"}, true},
		{"command with -h", []string{"--login", "-h"}, true},
		{"command with later --help only", []string{"--login", "codex", "--help"}, false},
		{"command without help", []string{"--status"}, false},
		{"profile run with --help", []string{"codex", "--help"}, false},
		{"profile run with -h", []string{"codex", "-h"}, false},
		{"bare help", []string{"--help"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := parseInvocation(tt.args)
			if err != nil {
				t.Fatalf("parseInvocation(%v): %v", tt.args, err)
			}
			if got := wantsHelp(inv); got != tt.want {
				t.Errorf("wantsHelp(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestHandleHelp covers the extracted decision run() relies on.
func TestHandleHelp(t *testing.T) {
	inv, err := parseInvocation([]string{"--models", "-h"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if !handleHelp(inv, &buf) {
		t.Error("handleHelp = false, want true for a command help request")
	}
	if !strings.Contains(buf.String(), "ccgp --models") {
		t.Errorf("output = %q", buf.String())
	}
	inv, err = parseInvocation([]string{"--models"})
	if err != nil {
		t.Fatal(err)
	}
	if handleHelp(inv, &buf) {
		t.Error("handleHelp = true for an invocation without a help argument")
	}
	inv, err = parseInvocation([]string{"codex", "--help"})
	if err != nil {
		t.Fatal(err)
	}
	if handleHelp(inv, &buf) {
		t.Error("handleHelp = true for a profile run")
	}
}

// TestRunPrintsCommandHelp drives the real run() dispatch: `--login --help`
// must print the command help and return without ensureConfig or the login
// command. The config env points at a fresh path; if the interception were
// removed, run() would create that file instead of printing help.
func TestRunPrintsCommandHelp(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.ktav")
	t.Setenv(config.ConfigFileEnv, cfgPath)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := run([]string{"--login", "--help"})
	w.Close()
	os.Stdout = old
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "ccgp --login <provider> [--device]") {
		t.Errorf("run printed no per-command help, got:\n%s", out)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("config file was created; the command ran instead of printing help (stat err %v)", err)
	}
}
