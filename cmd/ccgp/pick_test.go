package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
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
		{[]string{"up", "enter"}, claudeEndpointLabel}, // up from the first wraps to the last
		{[]string{"c"}, claudeEndpointLabel},
		{[]string{"4"}, claudeEndpointLabel},
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
	if err := launchPicked(nil); err == nil || !strings.Contains(err.Error(), "needs a terminal") ||
		!strings.Contains(err.Error(), "ccgp <profile>") {
		t.Errorf("launchPicked without a terminal: %v", err)
	}
}

// "." is a profile word for the parser, never a valid profile name.
func TestPickArgIsAProfileWordThatNoProfileCanHave(t *testing.T) {
	inv, err := parseInvocation([]string{pickArg, "-c", "x"})
	if err != nil || inv.cmd != nil || inv.profile != pickArg || strings.Join(inv.args, "|") != "-c|x" {
		t.Errorf("parseInvocation(. -c x) = %+v, %v", inv, err)
	}
	if err := config.ValidateProfileName(pickArg); err == nil {
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
	if err := runPicked(claudeEndpointLabel, []string{"-c"}); err == nil || err.Error() != "claude not found in PATH" {
		t.Errorf("claude picked, none in PATH: %v", err)
	}
	if err := runPicked("gone", nil); err == nil || !strings.Contains(err.Error(), `unknown profile "gone"`) {
		t.Errorf("a profile removed meanwhile: %v", err)
	}
}
