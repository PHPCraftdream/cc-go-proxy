package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/core"
	"github.com/PHPCraftdream/cc-go-proxy/internal/angel"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
)

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

// addClaude creates profile name of type claude in a fresh test config.
func addClaude(t *testing.T, name string) (string, *config.Config) {
	t.Helper()
	path, cfg := writeTestConfig(t, testConfig)
	if err := core.ProfileAdd(&bytes.Buffer{}, core.ProfileAddOpts{Type: config.ProviderTypeClaude, Name: name}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	return path, loadTestConfig(t)
}

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// testConfig covers one provider per interesting shape: codex (managed
// mapping), zai (preset, effort_map, api_key_env) and a rule-less profile.
const testConfig = `listen.host: 127.0.0.1
listen.port: 60093

providers: [
    {
        name: codex
        type: codex
    }
    {
        name: zai
        type: anthropic_compat
        base_url: https://api.z.ai/api/anthropic
        api_key_env: ZAI_KEY_TEST
        effort_map: {
            medium: high
            max: max
        }
    }
]

profiles: [
    {
        name: codex
        provider: codex
        ## ccgp:managed-models begin codex
        models: [
            {
                match: *sonnet*
                model: gpt-6-luna
                context_window: 872000
            }
            {
                match: *
                model: gpt-6-luna
                context_window: 872000
            }
        ]
        ## ccgp:managed-models end codex
    }
    {
        name: zai
        provider: zai
        models: [
            {
                match: *
                model: glm-5.3-flash
                context_window: 1000000
            }
        ]
    }
    {
        name: zai2
        provider: zai
    }
]
`

func profileByName(t *testing.T, cfg *config.Config, name string) *config.Profile {
	t.Helper()
	pf, ok := cfg.Profiles[name]
	if !ok {
		t.Fatalf("profile %q missing after the edit (have %v)", name, common.Names(cfg.Profiles))
	}
	return pf
}

// writeTestConfig writes a fixture and returns (path, loaded config).
func writeTestConfig(t *testing.T, text string) (string, *config.Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCGP_CONFIG", path)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("fixture does not load: %v", err)
	}
	return path, cfg
}

// isolate points every default location at empty temp dirs, so a test sees
// whether anything was written to the default layout, and clears CCGP_HOME and
// CCGP_CONFIG (restored afterwards, whatever run() sets).
func isolate(t *testing.T) (defaults string) {
	t.Helper()
	defaults = t.TempDir()
	for _, k := range []string{"APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "HOME"} {
		t.Setenv(k, defaults)
	}
	t.Setenv(home.EnvVar, "")
	t.Setenv("CCGP_CONFIG", "")
	t.Setenv("PROXY_TOKEN", "")
	return defaults
}

// --home puts the config in that directory and nothing in the default one; the
// variable is set so the background server started later finds the same home.
func TestHomeFlagMovesTheConfig(t *testing.T) {
	defaults := isolate(t)
	dir := filepath.Join(t.TempDir(), "other")
	if err := run([]string{"--home", dir, "--config", "reset"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.ktav"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "listen.port: 60093") || !strings.Contains(string(b), "listen.port: ") {
		t.Errorf("a new home must get a port of its own:\n%.300s", b)
	}
	if home.Root() != dir {
		t.Errorf("CCGP_HOME = %q, want %q", home.Root(), dir)
	}
	entries, _ := os.ReadDir(defaults)
	if len(entries) != 0 {
		t.Errorf("the default location was touched: %v", entries)
	}
}

func TestHomeFlagWithEquals(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "eq")
	if err := run([]string{"--home=" + dir, "--config", "reset"}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "config.ktav")) {
		t.Error("no config in the --home= directory")
	}
}

// --local makes ./.ccgp, and a .gitignore in it keeps the secrets out of git.
func TestLocalFlagMakesTheDirectoryHere(t *testing.T) {
	isolate(t)
	wd := t.TempDir()
	t.Chdir(wd)
	if err := run([]string{"--local", "--config", "reset"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"config.ktav", ".gitignore"} {
		if !exists(filepath.Join(wd, ".ccgp", f)) {
			t.Errorf(".ccgp/%s is missing", f)
		}
	}
}

// A project that has a .ccgp directory runs on it with no flag.
func TestProjectDirectoryIsUsedWithoutAFlag(t *testing.T) {
	defaults := isolate(t)
	wd := t.TempDir()
	t.Chdir(wd)
	if err := os.Mkdir(filepath.Join(wd, ".ccgp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--config", "reset"}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(wd, ".ccgp", "config.ktav")) {
		t.Error("the config was not written into ./.ccgp")
	}
	if entries, _ := os.ReadDir(defaults); len(entries) != 0 {
		t.Errorf("the default location was touched: %v", entries)
	}
}

// Without the directory and without a flag nothing changes: the default layout.
func TestNoHomeKeepsTheDefaultLayout(t *testing.T) {
	defaults := isolate(t)
	t.Chdir(t.TempDir())
	if err := run([]string{"--config", "reset"}); err != nil {
		t.Fatal(err)
	}
	if home.Root() != "" {
		t.Errorf("CCGP_HOME = %q", home.Root())
	}
	cfg, _ := home.Dir()
	if !strings.HasPrefix(cfg, defaults) || !exists(filepath.Join(cfg, "config.ktav")) {
		t.Errorf("config dir %q (default location %q)", cfg, defaults)
	}
}

// Help, the version and completion never make the directory.
func TestInfoCommandsDoNotCreateTheHome(t *testing.T) {
	isolate(t)
	wd := t.TempDir()
	t.Chdir(wd)
	for _, args := range [][]string{
		{"--local", "--version"},
		{"--local", "--help"},
		{"--local"},
		{"--local", "--status", "--help"},
		{"--local", "--login", "-h"},
		{"--local", "--complete", "ccgp", ""},
	} {
		if err := run(args); err != nil {
			t.Errorf("%q: %v", args, err)
		}
		if exists(filepath.Join(wd, ".ccgp")) {
			t.Fatalf("%q made ./.ccgp", args)
		}
	}
}

func TestHomeFlagErrors(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{
		{"--home"},
		{"--home", "--status"},
		{"--local", "--home", "x", "--version"},
		{"--local", "--local", "--version"},
	} {
		if err := run(args); err == nil {
			t.Errorf("run(%q) must fail", args)
		}
	}
}

func TestStatusShowsTheHome(t *testing.T) {
	var b strings.Builder
	if err := renderStatusText(&b, statusData{Home: "/p/.ccgp", LogFile: "/p/.ccgp/logs/ccgp.log"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "home:    /p/.ccgp\n") {
		t.Errorf("status:\n%s", b.String())
	}
	b.Reset()
	_ = renderStatusText(&b, statusData{})
	if strings.Contains(b.String(), "home:") {
		t.Errorf("the default layout prints no home line:\n%s", b.String())
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

// Config, secrets, Claude Code dirs and the log all follow the home.
func TestEveryPathFollowsTheHome(t *testing.T) {
	isolate(t)
	check := func(name, got string, want ...string) {
		t.Helper()
		if w := filepath.Join(want...); got != w {
			t.Errorf("%s = %q, want %q", name, got, w)
		}
	}
	base := func() (cfg, cr, claude, log string) {
		var err error
		if cfg, err = config.DefaultConfigPath(); err != nil {
			t.Fatal(err)
		}
		if cr, err = creds.DefaultDir(); err != nil {
			t.Fatal(err)
		}
		if claude, err = common.ClaudeHomeBase(); err != nil {
			t.Fatal(err)
		}
		if log, err = angel.LogPath(); err != nil {
			t.Fatal(err)
		}
		return
	}

	cfg, cr, cl, lg := base()
	confDir, _ := os.UserConfigDir()
	cacheDir, _ := os.UserCacheDir()
	app := filepath.Join(confDir, "cc-go-proxy")
	check("default config", cfg, app, "config.ktav")
	check("default creds", cr, app, "creds")
	check("default claude", cl, app, "claude")
	check("default log", lg, cacheDir, "cc-go-proxy", "logs", "ccgp.log")

	dir := t.TempDir()
	t.Setenv(home.EnvVar, dir)
	cfg, cr, cl, lg = base()
	check("config", cfg, dir, "config.ktav")
	check("creds", cr, dir, "creds")
	check("claude", cl, dir, "claude")
	check("log", lg, dir, "logs", "ccgp.log")
}

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := common.WriteJSON(&buf, map[string]int{"b": 2, "a": 1}); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"a\": 1,\n  \"b\": 2\n}\n"
	if buf.String() != want {
		t.Errorf("writeJSON = %q, want %q", buf.String(), want)
	}
}

func TestRenderStatusText(t *testing.T) {
	up := statusServer{Running: true, Version: "ccgp v9", PID: 42, URL: "http://127.0.0.1:1"}
	tests := []struct {
		name string
		data statusData
		want string
	}{
		{
			name: "down, file mode",
			data: statusData{
				Server:    statusServer{URL: "http://127.0.0.1:1"},
				LogFile:   "x.log",
				Config:    statusConfig{Mode: "file", Path: "x.ktav"},
				CredsDir:  "creds",
				Providers: []statusProvider{{Name: "p1", Type: "codex", Login: "not logged in"}},
				Profiles:  []statusProfile{{Name: "alpha", Provider: "p1"}},
			},
			want: "server:  not running at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  x.ktav\n" +
				"creds:   creds\n" +
				"  provider p1           codex   not logged in\n" +
				"  profile  alpha        -> p1\n",
		},
		{
			name: "up, env mode",
			data: statusData{
				Server:  up,
				LogFile: "x.log",
				Config:  statusConfig{Mode: "env"},
			},
			want: "server:  ccgp v9 running (pid 42) at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  env-only mode (no config.ktav)\n",
		},
		{
			name: "error mode",
			data: statusData{
				Server:  up,
				LogFile: "x.log",
				Config:  statusConfig{Mode: "error", Error: "parse: line 3"},
			},
			want: "server:  ccgp v9 running (pid 42) at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  error: parse: line 3\n",
		},
		{
			name: "missing mode",
			data: statusData{
				Server:  up,
				LogFile: "x.log",
				Config:  statusConfig{Mode: "missing", Path: "x.ktav"},
			},
			want: "server:  ccgp v9 running (pid 42) at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  x.ktav (not created yet; created on first run)\n",
		},
		{
			name: "file mode without creds dir",
			data: statusData{
				Server:  up,
				LogFile: "x.log",
				Config:  statusConfig{Mode: "file", Path: "x.ktav"},
			},
			want: "server:  ccgp v9 running (pid 42) at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  x.ktav\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := renderStatusText(&buf, tt.data); err != nil {
				t.Fatal(err)
			}
			if buf.String() != tt.want {
				t.Errorf("renderStatusText =\n%q\nwant\n%q", buf.String(), tt.want)
			}
		})
	}
}

// The fake secret must never reach the JSON payload; status only carries
// non-secret fields (paths, names, login descriptions).
const testSecretToken = "tok-SUPERSECRET-123"

func TestStatusJSON(t *testing.T) {
	t.Setenv("CCGP_TEST_SECRET", "sk-FAKEKEY-not-real-000")
	cfg := &config.Config{
		ConfigFile: filepath.Join(t.TempDir(), "x.ktav"),
		Providers: map[string]*config.Provider{
			"p1": {Name: "p1", Type: config.ProviderTypeAnthropicCompat, BaseURL: "http://127.0.0.1:9/" + testSecretToken, APIKeyEnv: "CCGP_TEST_SECRET"},
		},
		Profiles: map[string]*config.Profile{
			"alpha": {Name: "alpha", Provider: "p1"},
		},
	}
	providers := []statusProvider{{Name: "p1", Type: config.ProviderTypeAnthropicCompat, Login: "key from $CCGP_TEST_SECRET"}}
	profiles := []statusProfile{{Name: "alpha", Provider: "p1"}}
	st := buildStatus("http://127.0.0.1:1", "x.log", &angel.Health{Version: "ccgp v9", PID: 42}, nil, cfg, nil, filepath.Join(t.TempDir(), "creds"), nil, providers, profiles)

	var buf bytes.Buffer
	if err := common.WriteJSON(&buf, st); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, testSecretToken) {
		t.Error("JSON output contains the planted base URL token")
	}
	if strings.Contains(out, "sk-FAKEKEY-not-real-000") {
		t.Error("JSON output contains the API key env value")
	}

	var got statusData
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Server.Running || got.Server.PID != 42 || got.Server.URL != "http://127.0.0.1:1" {
		t.Errorf("server = %+v", got.Server)
	}
	if got.Config.Mode != "file" || got.Config.Path != cfg.ConfigFile {
		t.Errorf("config = %+v", got.Config)
	}
	if len(got.Providers) != 1 || got.Providers[0].Name != "p1" {
		t.Errorf("providers = %+v", got.Providers)
	}
	if len(got.Profiles) != 1 || got.Profiles[0].Provider != "p1" {
		t.Errorf("profiles = %+v", got.Profiles)
	}
}

func TestBuildProfileRowsAndText(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]*config.Provider{
			"p1": {Name: "p1", Type: config.ProviderTypeCodex, BaseURL: "http://127.0.0.1:9/" + testSecretToken},
			"p2": {Name: "p2", Type: config.ProviderTypeAnthropicCompat, APIKeyEnv: "CCGP_TEST_SECRET"},
		},
		Profiles: map[string]*config.Profile{
			"beta":  {Name: "beta", Provider: "p2"},
			"alpha": {Name: "alpha", Provider: "p1", Models: []config.ModelRule{{Match: "sonnet*", Model: "gpt-x"}}},
		},
	}
	store := creds.Store{Dir: filepath.Join(t.TempDir(), "creds")}
	t.Setenv("CCGP_TEST_SECRET", "sk-FAKEKEY-not-real-000")
	claudeBase := t.TempDir()
	if err := os.Mkdir(filepath.Join(claudeBase, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}

	rows := core.BuildProfileRows(cfg, store, claudeBase)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Name != "alpha" || rows[1].Name != "beta" {
		t.Errorf("row order = %s, %s", rows[0].Name, rows[1].Name)
	}
	if rows[0].MainModel != "gpt-x" || rows[1].MainModel != "-" {
		t.Errorf("main models = %q, %q", rows[0].MainModel, rows[1].MainModel)
	}
	if !rows[0].ClaudeDir || rows[1].ClaudeDir {
		t.Errorf("claudeDir = %v, %v", rows[0].ClaudeDir, rows[1].ClaudeDir)
	}
	if rows[0].Login != "not logged in (ccgp --login p1)" {
		t.Errorf("codex login = %q", rows[0].Login)
	}
	if rows[1].Login != "key from $CCGP_TEST_SECRET" {
		t.Errorf("compat login = %q", rows[1].Login)
	}

	var buf bytes.Buffer
	if err := core.ProfileList(&buf, rows); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, testSecretToken) || strings.Contains(out, "sk-FAKEKEY-not-real-000") {
		t.Error("text output contains a planted secret")
	}
	var want bytes.Buffer
	fmt.Fprintf(&want, "%-16s %-16s %-17s %-38s %-16s %s\n", "PROFILE", "PROVIDER", "TYPE", "LOGIN", "MAIN MODEL", "CLAUDE DIR")
	for _, r := range rows {
		dir := "no"
		if r.ClaudeDir {
			dir = "yes"
		}
		fmt.Fprintf(&want, "%-16s %-16s %-17s %-38s %-16s %s\n", r.Name, r.Provider, r.Type, r.Login, r.MainModel, dir)
	}
	if out != want.String() {
		t.Errorf("profileList output mismatch:\n%q\nwant\n%q", out, want.String())
	}
}

func TestProfileListJSONNoSecrets(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]*config.Provider{
			"p1": {Name: "p1", Type: config.ProviderTypeAnthropicCompat, BaseURL: "http://127.0.0.1:9/" + testSecretToken},
		},
		Profiles: map[string]*config.Profile{
			"alpha": {Name: "alpha", Provider: "p1", DefaultModel: "m"},
		},
	}
	rows := core.BuildProfileRows(cfg, creds.Store{Dir: t.TempDir()}, t.TempDir())
	var buf bytes.Buffer
	if err := common.WriteJSON(&buf, core.ProfileListData{Profiles: rows}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, testSecretToken) {
		t.Error("JSON output contains the planted base URL token")
	}
	var got core.ProfileListData
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Profiles) != 1 || got.Profiles[0].Name != "alpha" || got.Profiles[0].Provider != "p1" {
		t.Errorf("profiles = %+v", got.Profiles)
	}
}
