package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseInvocation(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantCmd     string   // "" for help (no command) or a profile run
		wantProfile string   // set for profile runs
		wantArgs    []string // remaining args
		wantErr     string   // "" means no error
	}{
		{"no args", nil, "", "", nil, ""},
		{"--help", []string{"--help"}, "", "", nil, ""},
		{"-h", []string{"-h"}, "", "", nil, ""},
		{"--serve with flags", []string{"--serve", "-d"}, "--serve", "", []string{"-d"}, ""},
		{"--start", []string{"--start"}, "--start", "", nil, ""},
		{"--stop", []string{"--stop"}, "--stop", "", nil, ""},
		{"--status", []string{"--status"}, "--status", "", nil, ""},
		{"--login with device", []string{"--login", "codex", "--device"}, "--login", "", []string{"codex", "--device"}, ""},
		{"--models apply", []string{"--models", "apply", "codex"}, "--models", "", []string{"apply", "codex"}, ""},
		{"--config reset", []string{"--config", "reset"}, "--config", "", []string{"reset"}, ""},
		{"--profile add", []string{"--profile", "add", "x", "--like", "codex"}, "--profile", "", []string{"add", "x", "--like", "codex"}, ""},
		{"--profile list", []string{"--profile", "list"}, "--profile", "", []string{"list"}, ""},
		{"--profile rm", []string{"--profile", "rm", "x", "--yes"}, "--profile", "", []string{"rm", "x", "--yes"}, ""},
		{"--send", []string{"--send", "codex", "claude", "3"}, "--send", "", []string{"codex", "claude", "3"}, ""},
		{"--import", []string{"--import", "zai"}, "--import", "", []string{"zai"}, ""},
		{"--export", []string{"--export", "zai", "2"}, "--export", "", []string{"zai", "2"}, ""},
		{"--version", []string{"--version"}, "--version", "", nil, ""},
		{"bare word is a profile", []string{"codex"}, "", "codex", nil, ""},
		{"profile args verbatim", []string{"codex", "-p", "x"}, "", "codex", []string{"-p", "x"}, ""},
		{"profile with dash flags", []string{"codex", "--status"}, "", "codex", []string{"--status"}, ""},
		{"former command is a profile", []string{"status"}, "", "status", nil, ""},
		{"login is a profile", []string{"login"}, "", "login", nil, ""},
		{"serve is a profile", []string{"serve", "-d"}, "", "serve", []string{"-d"}, ""},
		{"unknown command", []string{"--stauts"}, "", "", nil, "unknown command"},
		{"single dash is not a profile", []string{"-x"}, "", "", nil, "unknown command"},
		{"empty first argument", []string{""}, "", "", nil, "empty first argument"},
	}
	for _, tt := range tests {
		inv, err := parseInvocation(tt.args)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s: err = %v, want containing %q", tt.name, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		wantName := tt.wantCmd
		if inv.cmd == nil {
			if wantName != "" {
				t.Errorf("%s: got help/profile, want command %s", tt.name, wantName)
			}
		} else if inv.cmd.name != wantName {
			t.Errorf("%s: cmd = %s, want %s", tt.name, inv.cmd.name, wantName)
		}
		if inv.profile != tt.wantProfile {
			t.Errorf("%s: profile = %q, want %q", tt.name, inv.profile, tt.wantProfile)
		}
		if strings.Join(inv.args, "\x00") != strings.Join(tt.wantArgs, "\x00") {
			t.Errorf("%s: args = %q, want %q", tt.name, inv.args, tt.wantArgs)
		}
	}
}

func TestCommandTable(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range commands() {
		if seen[c.name] {
			t.Errorf("duplicate command %s", c.name)
		}
		seen[c.name] = true
		if c.desc == "" && !c.hidden {
			t.Errorf("%s: empty description", c.name)
		}
		if c.run == nil {
			t.Errorf("%s: nil run", c.name)
		}
	}
	for _, name := range []string{"--serve", "--start", "--stop", "--status", "--login", "--models", "--send", "--import", "--export", "--profile", "--config", "--version", "--help"} {
		if _, ok := findCommand(name); !ok {
			t.Errorf("findCommand(%s) not found", name)
		}
		if _, ok := findCommand(strings.TrimPrefix(name, "--")); ok {
			t.Errorf("bare word %q must not be a command", name)
		}
	}
}

func TestPrintHelpListsAllCommands(t *testing.T) {
	var buf bytes.Buffer
	printHelp(&buf)
	out := buf.String()
	for _, c := range commands() {
		if c.hidden {
			if strings.Contains(out, "ccgp "+c.name) {
				t.Errorf("help mentions hidden command %s", c.name)
			}
			continue
		}
		if !strings.Contains(out, "ccgp "+c.name) {
			t.Errorf("help does not mention %s", c.name)
		}
	}
	if !strings.Contains(out, "profile codex") {
		t.Error("help does not document the bare-word-is-a-profile rule")
	}
	if !strings.Contains(out, "ccgpy <profile>") {
		t.Error("help does not mention ccgpy")
	}
}

func TestAngelServeArgs(t *testing.T) {
	got := angelServeArgs([]string{"-d"})
	want := []string{"--serve", "-s", "-d"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("angelServeArgs = %q, want %q", got, want)
	}
}

// Commands that only read state must not create the default config.
func TestEnsureConfigPolicy(t *testing.T) {
	noConfig := map[string]bool{
		"--stop": true, "--status": true, "--config": true, "--logs": true, "--version": true, "--help": true,
		"--completion": true, "--complete": true, "--doctor": true, "--update": true,
	}
	for _, c := range commands() {
		if c.ensureConfig == noConfig[c.name] {
			t.Errorf("%s: ensureConfig = %v", c.name, c.ensureConfig)
		}
	}
}
