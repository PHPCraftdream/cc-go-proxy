package main

import (
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

var testNames = completionNames{
	profiles:  func() []string { return []string{"codex", "codex2", "zai"} },
	providers: func() []string { return []string{"codex", "zai"} },
}

func TestCompleteWords(t *testing.T) {
	tests := []struct {
		name  string
		words []string
		want  []string
	}{
		{"profiles and commands at the first word", []string{"ccgp", ""}, nil}, // checked separately below
		{"command prefix", []string{"ccgp", "--st"}, []string{"--start", "--status", "--stop"}},
		{"profile prefix", []string{"ccgp", "cod"}, []string{"codex", "codex2"}},
		{"profile run: rest belongs to claude", []string{"ccgp", "codex", "--"}, nil},
		{"profile add subcommand", []string{"ccgp", "--profile", ""}, []string{"add", "list", "rm"}},
		{"profile add flags", []string{"ccgp", "--profile", "add", "x", "--"}, []string{"--base-url", "--like", "--model", "--provider", "--type", "--window"}},
		{"profile add --like takes profiles", []string{"ccgp", "--profile", "add", "x", "--like", ""}, []string{"codex", "codex2", "zai"}},
		{"profile add --provider takes providers", []string{"ccgp", "--profile", "add", "x", "--provider", "c"}, []string{"codex"}},
		{"profile add --type", []string{"ccgp", "--profile", "add", "x", "--type", "co"}, []string{"codex", "codex_api"}},
		{"profile rm", []string{"ccgp", "--profile", "rm", "--p"}, []string{"--purge"}},
		{"profile list", []string{"ccgp", "--profile", "list", ""}, []string{"--json"}},
		{"login", []string{"ccgp", "--login", ""}, []string{"--device", "codex", "zai"}},
		{"login device only", []string{"ccgp", "--login", "codex", "--d"}, []string{"--device"}},
		{"logout", []string{"ccgp", "--logout", "z"}, []string{"zai"}},
		{"models", []string{"ccgp", "--models", "a"}, []string{"apply"}},
		{"which: profile then model", []string{"ccgp", "--which", "z"}, []string{"zai"}},
		{"which: after the profile only --json", []string{"ccgp", "--which", "zai", ""}, []string{"--json"}},
		{"send offers claude", []string{"ccgp", "--send", "cl"}, []string{"claude"}},
		{"send: no third completion", []string{"ccgp", "--send", "zai", "claude", ""}, nil},
		{"import", []string{"ccgp", "--import", "z"}, []string{"zai"}},
		{"config", []string{"ccgp", "--config", ""}, []string{"reset"}},
		{"connections words", []string{"ccgp", "--connections", ""}, []string{"--json", "active", "proxy", "total"}},
		{"connections: numbers are free text", []string{"ccgp", "--connections", "active", ""}, nil},
		{"connections proxy values", []string{"ccgp", "--connections", "proxy", ""}, []string{"http://", "inherit", "none", "off", "socks5://"}},
		{"connections: profile may follow the proxy value", []string{"ccgp", "--connections", "proxy", "none", ""}, []string{"profile"}},
		{"connections: profile takes profiles", []string{"ccgp", "--connections", "proxy", "none", "profile", "cod"}, []string{"codex", "codex2"}},
		{"completion shells", []string{"ccgp", "--completion", ""}, []string{"bash", "powershell", "zsh"}},
		{"logs flags", []string{"ccgp", "--logs", "-"}, []string{"-f", "-n"}},
		{"unknown command", []string{"ccgp", "--nope", ""}, nil},
		{"empty marker is an empty word", []string{"ccgp", "--profile", emptyWord}, []string{"add", "list", "rm"}},
		{"program path and .exe", []string{`C:\bin\ccgp.exe`, "--sta"}, []string{"--start", "--status"}},
		{"ccgpy.exe is ccgpy", []string{"ccgpy.exe", "z"}, []string{"zai"}},
		{"ccgpy completes profiles only", []string{"ccgpy", ""}, []string{"codex", "codex2", "zai"}},
		{"ccgpy: nothing after the profile", []string{"ccgpy", "codex", ""}, nil},
		{"program only", []string{"ccgp"}, nil},
	}
	for _, tt := range tests {
		got := completeWords(tt.words, testNames)
		if tt.name == "profiles and commands at the first word" {
			// Every visible command plus every profile, sorted, no hidden rows.
			want := append(commandNames(), "codex", "codex2", "zai")
			if len(got) != len(want) {
				t.Errorf("%s: %d candidates, want %d", tt.name, len(got), len(want))
			}
			for _, w := range want {
				if !contains(got, w) {
					t.Errorf("%s: missing %q", tt.name, w)
				}
			}
			if contains(got, "--complete") {
				t.Errorf("%s: hidden command offered", tt.name)
			}
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestCompleteWordsWithoutNames(t *testing.T) {
	// No config: commands still complete, names are simply absent.
	got := completeWords([]string{"ccgp", "--lo"}, completionNames{})
	want := []string{"--login", "--logout", "--logs"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := completeWords([]string{"ccgp", "--login", ""}, completionNames{}); !reflect.DeepEqual(got, []string{"--device"}) {
		t.Errorf("login without providers: %q", got)
	}
}

func TestCompletionCmdScripts(t *testing.T) {
	for shell, parts := range map[string][]string{
		"bash":       {"complete -F _ccgp_complete ccgp ccgpy", "--complete"},
		"zsh":        {"#compdef ccgp ccgpy", "--complete", "compdef _ccgp_complete ccgp ccgpy"},
		"powershell": {"Register-ArgumentCompleter -Native -CommandName ccgp, ccgpy", "--complete", emptyWord},
	} {
		out := captureStdout(t, func() {
			if err := completionCmd([]string{shell}); err != nil {
				t.Errorf("%s: %v", shell, err)
			}
		})
		for _, p := range parts {
			if !strings.Contains(out, p) {
				t.Errorf("%s script lacks %q:\n%s", shell, p, out)
			}
		}
	}
}

func TestCompletionCmdUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"fish"}, {"bash", "zsh"}} {
		err := completionCmd(args)
		if err == nil || !strings.Contains(err.Error(), "usage: ccgp --completion bash|zsh|powershell") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestCompleteIsHiddenAndQuiet(t *testing.T) {
	c, ok := findCommand("--complete")
	if !ok || !c.hidden || c.ensureConfig {
		t.Fatalf("--complete: found=%v hidden=%v ensureConfig=%v", ok, c.hidden, c.ensureConfig)
	}
	if err := completeCmd(nil); err != nil {
		t.Errorf("completeCmd(nil) = %v", err)
	}
}

// captureStdout returns what fn prints to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}
