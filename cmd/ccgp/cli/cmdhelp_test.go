package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
)

func TestCommandHelpTextHasEntryOrFallback(t *testing.T) {
	for _, c := range Commands() {
		var buf bytes.Buffer
		printCommandHelp(&buf, c)
		out := buf.String()
		if out == "" {
			t.Errorf("%s: empty help text", c.Name)
		}
		if !strings.Contains(out, c.Name) {
			t.Errorf("%s: help text does not mention the command:\n%s", c.Name, out)
		}
		if _, ok := CommandHelp[c.Name]; !ok && !strings.Contains(out, c.Desc) {
			t.Errorf("%s: fallback help must contain the one-line desc, got:\n%s", c.Name, out)
		}
	}
}

// Every visible command has its own long help with an example, so a new
// command cannot ship with the one-line fallback only.
func TestEveryVisibleCommandHasFullHelpWithExample(t *testing.T) {
	for _, c := range Commands() {
		if c.Hidden {
			continue
		}
		text, ok := CommandHelp[c.Name]
		if !ok {
			t.Errorf("%s: no entry in commandHelp", c.Name)
			continue
		}
		if !strings.Contains(text, "Example") {
			t.Errorf("%s: help has no example:\n%s", c.Name, text)
		}
		if !strings.HasPrefix(text, "ccgp "+c.Name) {
			t.Errorf("%s: help does not start with the command line:\n%s", c.Name, text)
		}
	}
}

// The long help names the flags the command's synopsis lists.
func TestCommandHelpMentionsSynopsisFlags(t *testing.T) {
	want := map[string][]string{
		"--status":      {"--json"},
		"--models":      {"--json", "apply", "-v"},
		"--which":       {"--json"},
		"--logs":        {"-f", "-n"},
		"--profile":     {"--like", "--provider", "--type", "--purge", "--yes", "--json", "claude"},
		"--connections": {"active", "total", "proxy", "--json"},
		"--peak":        {"set", "add", "rm", "tz", "clear", "pause", "resume", "--json"},
		"--doctor":      {"--offline", "--json"},
		"--update":      {"--check"},
		"--login":       {"--device", "claude"},
		"--serve":       {"-d", "-s"},
		"--send":        {"[--fork] [--force]", "--fork --force"},
		"--import":      {"[--fork] [--force]", "--fork --force"},
		"--export":      {"[--fork] [--force]", "--fork --force"},
	}
	for name, flags := range want {
		text := CommandHelp[name]
		for _, f := range flags {
			if !strings.Contains(text, f) {
				t.Errorf("%s: help does not mention %q", name, f)
			}
		}
	}
}

func TestCommandHelpTextFallback(t *testing.T) {
	c := Command{Name: "--nosuch", Args: "", Desc: "Restart the background server"}
	var buf bytes.Buffer
	printCommandHelp(&buf, c)
	want := "ccgp --nosuch\n\nRestart the background server\n"
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
			inv, err := ParseInvocation(tt.args)
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
	inv, err := ParseInvocation([]string{"--models", "-h"})
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
	inv, err = ParseInvocation([]string{"--models"})
	if err != nil {
		t.Fatal(err)
	}
	if handleHelp(inv, &buf) {
		t.Error("handleHelp = true for an invocation without a help argument")
	}
	inv, err = ParseInvocation([]string{"codex", "--help"})
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

// --fork and --force work together; no text offers them as alternatives.
func TestNoHelpOffersForkAndForceAsAlternatives(t *testing.T) {
	for _, c := range Commands() {
		if strings.Contains(c.Args, "[--fork|--force]") {
			t.Errorf("%s: synopsis offers --fork and --force as alternatives: %q", c.Name, c.Args)
		}
	}
	for name, text := range CommandHelp {
		if strings.Contains(text, "[--fork|--force]") {
			t.Errorf("%s: help offers --fork and --force as alternatives", name)
		}
	}
}

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
		inv, err := ParseInvocation(tt.args)
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
		if inv.Cmd == nil {
			if wantName != "" {
				t.Errorf("%s: got help/profile, want command %s", tt.name, wantName)
			}
		} else if inv.Cmd.Name != wantName {
			t.Errorf("%s: cmd = %s, want %s", tt.name, inv.Cmd.Name, wantName)
		}
		if inv.Profile != tt.wantProfile {
			t.Errorf("%s: profile = %q, want %q", tt.name, inv.Profile, tt.wantProfile)
		}
		if strings.Join(inv.Args, "\x00") != strings.Join(tt.wantArgs, "\x00") {
			t.Errorf("%s: args = %q, want %q", tt.name, inv.Args, tt.wantArgs)
		}
	}
}

func TestCommandTable(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Commands() {
		if seen[c.Name] {
			t.Errorf("duplicate command %s", c.Name)
		}
		seen[c.Name] = true
		if c.Desc == "" && !c.Hidden {
			t.Errorf("%s: empty description", c.Name)
		}
		if c.Run == nil {
			t.Errorf("%s: nil run", c.Name)
		}
	}
	for _, name := range []string{"--serve", "--start", "--stop", "--status", "--login", "--models", "--send", "--import", "--export", "--profile", "--config", "--version", "--help"} {
		if _, ok := FindCommand(name); !ok {
			t.Errorf("findCommand(%s) not found", name)
		}
		if _, ok := FindCommand(strings.TrimPrefix(name, "--")); ok {
			t.Errorf("bare word %q must not be a command", name)
		}
	}
}

func TestPrintHelpListsAllCommands(t *testing.T) {
	var buf bytes.Buffer
	printHelp(&buf)
	out := buf.String()
	for _, c := range Commands() {
		if c.Hidden {
			if strings.Contains(out, "ccgp "+c.Name) {
				t.Errorf("help mentions hidden command %s", c.Name)
			}
			continue
		}
		if !strings.Contains(out, "ccgp "+c.Name) {
			t.Errorf("help does not mention %s", c.Name)
		}
	}
	if !strings.Contains(out, "profile codex") {
		t.Error("help does not document the bare-word-is-a-profile rule")
	}
	if !strings.Contains(out, "ccgpy <profile>") {
		t.Error("help does not mention ccgpy")
	}
}

func TestHelpDescribesTheHome(t *testing.T) {
	var b strings.Builder
	printHelp(&b)
	for _, want := range []string{"--local", "--home <dir>", "CCGP_HOME", ".ccgp", "ccgp --local --login codex"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

var testNames = CompletionNames{
	Profiles:  func() []string { return []string{"codex", "codex2", "zai"} },
	Providers: func() []string { return []string{"codex", "zai"} },
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
		{"profile add subcommand", []string{"ccgp", "--profile", ""}, []string{"add", "list", "rm", "set", "unset"}},
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
		{"send: after the two sides only the flags", []string{"ccgp", "--send", "zai", "claude", ""}, []string{"--force", "--fork"}},
		{"send: flags beside the sides", []string{"ccgp", "--send", "--f"}, []string{"--force", "--fork"}},
		{"import", []string{"ccgp", "--import", "z"}, []string{"zai"}},
		{"import: flags and profiles", []string{"ccgp", "--import", ""}, []string{"--force", "--fork", "codex", "codex2", "zai"}},
		{"import: after the profile only the flags", []string{"ccgp", "--import", "zai", ""}, []string{"--force", "--fork"}},
		{"export: --fork", []string{"ccgp", "--export", "codex", "--fork"}, []string{"--fork"}},
		{"config", []string{"ccgp", "--config", ""}, []string{"reset"}},
		{"connections words", []string{"ccgp", "--connections", ""}, []string{"--json", "active", "proxy", "total"}},
		{"connections: numbers are free text", []string{"ccgp", "--connections", "active", ""}, nil},
		{"connections proxy values", []string{"ccgp", "--connections", "proxy", ""}, []string{"http://", "inherit", "none", "off", "socks5://"}},
		{"connections: a provider or a profile may follow the proxy value", []string{"ccgp", "--connections", "proxy", "none", ""}, []string{"profile", "provider"}},
		{"connections: profile takes profiles", []string{"ccgp", "--connections", "proxy", "none", "profile", "cod"}, []string{"codex", "codex2"}},
		{"connections: provider takes providers", []string{"ccgp", "--connections", "proxy", "none", "provider", ""}, []string{"codex", "zai"}},
		{"speed: profiles", []string{"ccgp", "--speed", "cod"}, []string{"codex", "codex2"}},
		{"speed: family", []string{"ccgp", "--speed", "codex", ""}, []string{"astra", "luna", "sol", "terra"}},
		{"speed: policy", []string{"ccgp", "--speed", "codex", "astra", ""}, []string{"fast", "inherit", "standard", "unset"}},
		{"peak: providers", []string{"ccgp", "--peak", ""}, []string{"--json", "codex", "zai"}},
		{"peak: verbs", []string{"ccgp", "--peak", "zai", ""}, []string{"--json", "add", "clear", "pause", "resume", "rm", "set", "tz"}},
		{"peak: zones", []string{"ccgp", "--peak", "zai", "tz", ""}, []string{"+08:00", "Asia/Shanghai", "UTC", "local"}},
		{"peak: days", []string{"ccgp", "--peak", "zai", "set", "m"}, []string{"mon-fri"}},
		{"peak: nothing after pause", []string{"ccgp", "--peak", "zai", "pause", ""}, nil},
		{"completion shells", []string{"ccgp", "--completion", ""}, []string{"bash", "powershell", "zsh"}},
		{"logs flags", []string{"ccgp", "--logs", "-"}, []string{"-f", "-n"}},
		{"unknown command", []string{"ccgp", "--nope", ""}, nil},
		{"empty marker is an empty word", []string{"ccgp", "--profile", emptyWord}, []string{"add", "list", "rm", "set", "unset"}},
		{"program path and .exe", []string{`C:\bin\ccgp.exe`, "--sta"}, []string{"--start", "--status"}},
		{"ccgpy.exe is ccgpy", []string{"ccgpy.exe", "z"}, []string{"zai"}},
		{"ccgpy completes the home flags and profiles", []string{"ccgpy", ""}, []string{"--home", "--local", "codex", "codex2", "zai"}},
		{"ccgpy: nothing after the profile", []string{"ccgpy", "codex", ""}, nil},
		{"program only", []string{"ccgp"}, nil},
	}
	for _, tt := range tests {
		got := CompleteWords(tt.words, testNames)
		if tt.name == "profiles and commands at the first word" {
			// Every visible command, the home flags and every profile, no hidden rows.
			want := append(commandNames(), "--local", "--home", "codex", "codex2", "zai")
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

func TestCompleteWordsWithoutNames(t *testing.T) {
	// No config: commands still complete, names are simply absent.
	got := CompleteWords([]string{"ccgp", "--lo"}, CompletionNames{})
	want := []string{"--local", "--login", "--logout", "--logs"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := CompleteWords([]string{"ccgp", "--login", ""}, CompletionNames{}); !reflect.DeepEqual(got, []string{"--device"}) {
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
	c, ok := FindCommand("--complete")
	if !ok || !c.Hidden || c.EnsureConfig {
		t.Fatalf("--complete: found=%v hidden=%v ensureConfig=%v", ok, c.Hidden, c.EnsureConfig)
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

func TestCompletionOffersAndSkipsTheHomeFlags(t *testing.T) {
	has := func(got []string, w string) bool {
		for _, g := range got {
			if g == w {
				return true
			}
		}
		return false
	}
	if got := CompleteWords([]string{"ccgp", "--ho"}, testNames); !reflect.DeepEqual(got, []string{"--home"}) {
		t.Errorf("--ho: %q", got)
	}
	first := CompleteWords([]string{"ccgp", ""}, testNames)
	for _, w := range []string{"--local", "--home", "--status", "codex"} {
		if !has(first, w) {
			t.Errorf("first word lacks %q: %q", w, first)
		}
	}
	// after the flag the commands and profiles follow, the flags are not offered twice
	after := CompleteWords([]string{"ccgp", "--local", ""}, testNames)
	if has(after, "--local") || has(after, "--home") || !has(after, "--status") || !has(after, "zai") {
		t.Errorf("after --local: %q", after)
	}
	if got := CompleteWords([]string{"ccgp", "--home", "d", "--st"}, testNames); !reflect.DeepEqual(got, []string{"--start", "--status", "--stop"}) {
		t.Errorf("after --home d: %q", got)
	}
	if got := CompleteWords([]string{"ccgp", "--local", "--login", ""}, testNames); !reflect.DeepEqual(got, []string{"--device", "codex", "zai"}) {
		t.Errorf("a command after --local: %q", got)
	}
	// the directory is the shell's to complete
	if got := CompleteWords([]string{"ccgp", "--home", ""}, testNames); got != nil {
		t.Errorf("after --home: %q", got)
	}
	// ccgpy: profiles, and the flags first
	if got := CompleteWords([]string{"ccgpy", "--local", "co"}, testNames); !reflect.DeepEqual(got, []string{"codex", "codex2"}) {
		t.Errorf("ccgpy: %q", got)
	}
	if got := CompleteWords([]string{"ccgpy", "--l"}, testNames); !reflect.DeepEqual(got, []string{"--local"}) {
		t.Errorf("ccgpy flags: %q", got)
	}
}

// Completion reads the names of the home the typed flags choose, and writes nothing.
func TestCompleteCmdUsesTheChosenHome(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "h")
	if err := run([]string{"--home", dir, "--config", "reset"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvVar, "")
	other := filepath.Join(t.TempDir(), "never")
	out := captureStdout(t, func() {
		if err := run([]string{"--complete", "ccgp", "--home", dir, "co"}); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "codex") {
		t.Errorf("no profile of the chosen home:\n%s", out)
	}
	captureStdout(t, func() { _ = run([]string{"--complete", "ccgp", "--home", other, "--stat"}) })
	if exists(other) {
		t.Error("completion made the home directory")
	}
}
