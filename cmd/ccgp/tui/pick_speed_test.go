package tui

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/cli"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/sess"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/tune"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgedit"
)

// pickTUI is the list of `ccgp .` over uiConfig (profiles codex, work, zai).
func pickTUI(t *testing.T) (*tui, *string) {
	t.Helper()
	h := newTUI(t, uiConfig)
	picked := new(string)
	h.a.stack = []screen{pickMenu(picked)}
	return h, picked
}

func TestPickListsTheProfilesAndYourOwnClaude(t *testing.T) {
	h, _ := pickTUI(t)
	h.has("Run Claude Code with which profile?", "1 codex codex codex", "2 work work claude", "3 zai zai anthropic_compat",
		"4 claude your own Claude Code, as plain claude (no ccgp)",
		"c your own Claude Code · esc, q or ctrl+c leave without running")
	h.lacks("esc back")
}

func TestPickChoosesAndQuits(t *testing.T) {
	for _, c := range []struct {
		keys []string
		want string
	}{
		{[]string{"2"}, "work"},
		{[]string{"enter"}, "codex"},
		{[]string{"down", "down", "enter"}, "zai"},
		{[]string{"up", "enter"}, sess.ClaudeEndpointLabel}, // up from the first wraps to the last
		{[]string{"c"}, sess.ClaudeEndpointLabel},
		{[]string{"4"}, sess.ClaudeEndpointLabel},
	} {
		h, picked := pickTUI(t)
		h.press(c.keys...)
		if *picked != c.want || !h.a.quit {
			t.Errorf("%q: picked %q, quit %v; want %q", c.keys, *picked, h.a.quit, c.want)
		}
		if h.text() != uiConfig {
			t.Errorf("%q: choosing changed the config", c.keys)
		}
	}
}

func TestPickClosedWithoutAChoice(t *testing.T) {
	for _, k := range []string{"esc", "q", "ctrl+c"} {
		h, picked := pickTUI(t)
		h.press(k)
		if *picked != "" || !h.a.quit {
			t.Errorf("%q: picked %q, quit %v; want nothing and quit", k, *picked, h.a.quit)
		}
	}
}

// Without a terminal there is no list to draw: the error says to name a profile.
func TestLaunchPickedNeedsATerminal(t *testing.T) {
	newTUI(t, uiConfig)
	if err := LaunchPicked(nil); err == nil || !strings.Contains(err.Error(), "needs a terminal") ||
		!strings.Contains(err.Error(), "ccgp <profile>") {
		t.Errorf("launchPicked without a terminal: %v", err)
	}
}

// "." is a profile word for the parser, never a valid profile name.
func TestPickArgIsAProfileWordThatNoProfileCanHave(t *testing.T) {
	inv, err := cli.ParseInvocation([]string{cli.PickArg, "-c", "x"})
	if err != nil || inv.Cmd != nil || inv.Profile != cli.PickArg || strings.Join(inv.Args, "|") != "-c|x" {
		t.Errorf("parseInvocation(. -c x) = %+v, %v", inv, err)
	}
	if err := cfgedit.ValidateProfileName(cli.PickArg); err == nil {
		t.Error(`"." must not be a valid profile name`)
	}
}

// What the list returned is run: nothing, plain claude (found in PATH) or the
// profile (launch, which names an unknown one).
func TestRunPicked(t *testing.T) {
	newTUI(t, uiConfig)
	if err := runPicked("", nil); !errors.Is(err, errNotPicked) {
		t.Errorf("nothing picked: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if err := runPicked(sess.ClaudeEndpointLabel, []string{"-c"}); err == nil || err.Error() != "claude not found in PATH" {
		t.Errorf("claude picked, none in PATH: %v", err)
	}
	if err := runPicked("gone", nil); err == nil || !strings.Contains(err.Error(), `unknown profile "gone"`) {
		t.Errorf("a profile removed meanwhile: %v", err)
	}
}

const peakTestBody = "listen.port: 60093\n\nproviders: [\n    {\n        name: p\n        type: codex\n    }\n    {\n        name: work\n        type: claude\n    }\n]\n\nprofiles: [\n    {\n        name: pp\n        provider: p\n    }\n    {\n        name: work\n        provider: work\n    }\n]\n"

func peakTestConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(p, []byte(peakTestBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, p)
	return p
}

func TestSpeedTUIUnsetParityAndRepeat(t *testing.T) {
	h := newTUI(t, parityConfig)
	if err := tune.Speed(&bytes.Buffer{}, []string{"codex", "astra", "fast"}); err != nil {
		t.Fatal(err)
	}
	if err := h.a.reload(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(h.cfgPath + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	h.open("config", "speed", "profile:codex", "family:astra")
	if !strings.Contains(h.view(), "fast (profile rule)") {
		t.Fatalf("inspection: %s", h.view())
	}
	inspected, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInspect, err := filepath.Glob(h.cfgPath + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, inspected) || len(backups) != len(afterInspect) {
		t.Fatal("inspection wrote config or backup")
	}
	h.open("unset")
	if h.a.errMsg != "" {
		t.Fatal(h.a.errMsg)
	}
	viaTUI, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	backups, err = filepath.Glob(h.cfgPath + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	h.open("unset")
	if h.a.errMsg != "" {
		t.Fatal(h.a.errMsg)
	}
	if !strings.Contains(h.a.msg, "unchanged") {
		t.Fatalf("repeated unset: %q", h.a.msg)
	}
	repeated, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	afterRepeat, err := filepath.Glob(h.cfgPath + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viaTUI, repeated) || len(backups) != len(afterRepeat) {
		t.Fatal("repeated TUI unset wrote config or backup")
	}
	path := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, path)
	if err := tune.Speed(&bytes.Buffer{}, []string{"codex", "astra", "unset"}); err != nil {
		t.Fatal(err)
	}
	viaCLI, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viaTUI, viaCLI) {
		t.Fatal("TUI and CLI unset produced different configs")
	}
	backups, err = filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := tune.Speed(&bytes.Buffer{}, []string{"codex", "astra", "unset"}); err != nil {
		t.Fatal(err)
	}
	repeated, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterRepeat, err = filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viaCLI, repeated) || len(backups) != len(afterRepeat) {
		t.Fatal("repeated CLI unset wrote config or backup")
	}
}

func TestSpeedProfileIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ktav")
	body := strings.Replace(parityConfig, "name: zai2\n        provider: zai", "name: zai2\n        provider: codex", 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, path)
	if err := tune.Speed(&bytes.Buffer{}, []string{"codex", "astra", "fast"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tune.Speed(&out, []string{"zai2", "astra"}); err != nil || !strings.Contains(out.String(), "inherit (client request)") {
		t.Fatalf("other profile: %q %v", out.String(), err)
	}
}

func TestSpeedRulesCommand(t *testing.T) {
	path := peakTestConfig(t)
	run := func(args ...string) (string, error) {
		var b bytes.Buffer
		err := tune.Speed(&b, args)
		return b.String(), err
	}
	for _, args := range [][]string{{"pp"}, {"work", "astra"}, {"missing", "astra"}, {"pp", "astra-1", "fast"}, {"pp", "astra", "turbo"}} {
		if _, err := run(args...); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if out, err := run("pp", "astra"); err != nil || !strings.Contains(out, "inherit (client request)") {
		t.Fatalf("inspect: %q %v", out, err)
	}
	if out, err := run("pp", "astra", "fast"); err != nil || !strings.Contains(out, "fast (profile rule)") {
		t.Fatalf("set: %q %v", out, err)
	}
	if out, err := run(); err != nil || !strings.Contains(out, "pp astra: fast") {
		t.Fatalf("list: %q %v", out, err)
	}
	if out, err := run("pp", "sol", "standard"); err != nil || !strings.Contains(out, "standard") {
		t.Fatalf("set standard: %q %v", out, err)
	}
	if _, err := run("pp", "astra", "inherit"); err != nil {
		t.Fatal(err)
	}
	if out, err := run("pp", "astra"); err != nil || !strings.Contains(out, "inherit") {
		t.Fatalf("inspect unset: %q %v", out, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := run("pp", "astra", "unset"); err != nil || !strings.Contains(out, "unchanged") {
		t.Fatalf("repeat unset: %q %v", out, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || len(backups) != len(next) {
		t.Fatal("repeat unset wrote config or backup")
	}
	if out, err := run(); err != nil || strings.Contains(out, "astra") || !strings.Contains(out, "pp sol: standard") {
		t.Fatalf("list after unset: %q %v", out, err)
	}
}
