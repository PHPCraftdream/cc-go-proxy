package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/angel"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
)

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
	if got := completeWords([]string{"ccgp", "--ho"}, testNames); !reflect.DeepEqual(got, []string{"--home"}) {
		t.Errorf("--ho: %q", got)
	}
	first := completeWords([]string{"ccgp", ""}, testNames)
	for _, w := range []string{"--local", "--home", "--status", "codex"} {
		if !has(first, w) {
			t.Errorf("first word lacks %q: %q", w, first)
		}
	}
	// after the flag the commands and profiles follow, the flags are not offered twice
	after := completeWords([]string{"ccgp", "--local", ""}, testNames)
	if has(after, "--local") || has(after, "--home") || !has(after, "--status") || !has(after, "zai") {
		t.Errorf("after --local: %q", after)
	}
	if got := completeWords([]string{"ccgp", "--home", "d", "--st"}, testNames); !reflect.DeepEqual(got, []string{"--start", "--status", "--stop"}) {
		t.Errorf("after --home d: %q", got)
	}
	if got := completeWords([]string{"ccgp", "--local", "--login", ""}, testNames); !reflect.DeepEqual(got, []string{"--device", "codex", "zai"}) {
		t.Errorf("a command after --local: %q", got)
	}
	// the directory is the shell's to complete
	if got := completeWords([]string{"ccgp", "--home", ""}, testNames); got != nil {
		t.Errorf("after --home: %q", got)
	}
	// ccgpy: profiles, and the flags first
	if got := completeWords([]string{"ccgpy", "--local", "co"}, testNames); !reflect.DeepEqual(got, []string{"codex", "codex2"}) {
		t.Errorf("ccgpy: %q", got)
	}
	if got := completeWords([]string{"ccgpy", "--l"}, testNames); !reflect.DeepEqual(got, []string{"--local"}) {
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
		if claude, err = claudeHomeBase(); err != nil {
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
