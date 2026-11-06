package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

// The command line and the interface (`ccgp --interactive`) do the same things,
// both ways: whatever a command can do the interface can, and whatever the
// interface offers a command offers. This file keeps it so.
//
//   - Every command and every verb of it (command.verbs) is an "entry"; every
//     item of every screen is an "id path" found by crawling the interface.
//   - features ties entries to id paths. An entry with no path, or a path (that
//     does something: asks, acts, edits) with no entry, fails the test, and so
//     does a stale line of the table. What cannot be in the interface is in
//     cliOnly, with the reason.
//   - For every feature that edits, the command and the keys of the interface
//     are run on the same config and must leave the same file; for every
//     feature that runs a command, the interface must start exactly that one.
//
// A new command, verb or screen item fails here until the other side exists.

// cliOnly are the commands the interface does not have, and why.
var cliOnly = map[string]string{
	"--serve":       "runs the server in the foreground, which is the opposite of a menu; the interface starts and stops the background one",
	"--completion":  "prints a script for the shell; there is no shell in a full-screen interface",
	"--help":        "the interface shows its keys on every screen and each item says what it does",
	"--interactive": "the interface itself",
	"-i":            "the interface itself",
}

// feature ties the entries of the command line to the id paths of the
// interface that do the same.
type feature struct {
	cli []string
	tui []string
}

// peakAt are the two places where the peak hours of a provider are edited.
var peakAt = []string{"peak/provider", "providers/provider/peak"}

func under(prefixes []string, tail string) []string {
	var out []string
	for _, p := range prefixes {
		out = append(out, p+"/"+tail)
	}
	return out
}

func backendKinds(prefix string) []string {
	var out []string
	for _, k := range []string{"codex", "codex_api", "anthropic_compat", "claude", "claude_api"} {
		out = append(out, prefix+"/kind_"+k)
	}
	return out
}

func features() []feature {
	f := []feature{
		{[]string{"--profile list"}, []string{"profiles", "profiles/profile"}},
		{[]string{"--profile add --like"}, []string{"profiles/add/add_like/name"}},
		{[]string{"--profile add --provider"}, []string{"profiles/add/add_provider/name"}},
		{[]string{"--profile add --type"}, append(backendKinds("profiles/add/add_type"), backendKinds("providers/add")...)},
		{[]string{"--profile rm"}, []string{"profiles/profile/rm/rm_keep", "profiles/profile/rm/rm_purge"}},
		{[]string{"--profile set default_model", "--profile unset default_model"}, []string{"profiles/profile/default_model"}},
		{[]string{"--profile set context_window", "--profile unset context_window"}, []string{"profiles/profile/context_window"}},
		{[]string{"--profile set compact_percent", "--profile unset compact_percent"}, []string{"profiles/profile/compact_percent"}},
		{[]string{"--profile set provider"}, []string{"profiles/profile/move/provider"}},

		{[]string{"--provider"}, []string{"providers", "providers/provider"}},
		{[]string{"--provider set base_url", "--provider unset base_url"}, []string{"providers/provider/base_url", "profiles/profile/base_url"}},
		{[]string{"--provider set api_key_env", "--provider unset api_key_env"}, []string{"providers/provider/api_key_env"}},

		{[]string{"--connections"}, []string{"config"}},
		{[]string{"--connections active"}, []string{"config/active"}},
		{[]string{"--connections total"}, []string{"config/total"}},
		{[]string{"--connections proxy"}, []string{"config/proxy"}},
		{[]string{"--connections proxy provider"}, []string{"providers/provider/proxy"}},
		{[]string{"--connections proxy profile"}, []string{"profiles/profile/proxy"}},

		{[]string{"--peak"}, append([]string{"peak"}, peakAt...)},
		{[]string{"--peak set"}, under(peakAt, "set")},
		{[]string{"--peak add"}, under(peakAt, "add")},
		{[]string{"--peak rm"}, under(peakAt, "rm/window")},
		{[]string{"--peak tz"}, under(peakAt, "tz")},
		{[]string{"--peak clear"}, under(peakAt, "clear")},
		{[]string{"--peak pause"}, under(peakAt, "pause")},
		{[]string{"--peak resume"}, under(peakAt, "resume")},

		{[]string{"--models"}, []string{"models/show", "models/profile", "models/profile/show"}},
		{[]string{"--models apply"}, []string{"models/apply", "models/profile/apply"}},
		{[]string{"--which"}, []string{"models/profile/which"}},
		{[]string{"--config reset"}, []string{"config/reset"}},

		{[]string{"--login"}, []string{"providers/provider/login", "profiles/profile/login"}},
		{[]string{"--login --device"}, []string{"providers/provider/login_device"}},
		{[]string{"--logout"}, []string{"providers/provider/logout"}},

		{[]string{"--send"}, []string{"sessions/send/name/name/copy"}},
		{[]string{"--send --fork"}, []string{"sessions/send/name/name/fork"}},
		{[]string{"--send --force"}, []string{"sessions/send/name/name/force"}},
		{[]string{"--import"}, []string{"sessions/import/name/copy"}},
		{[]string{"--import --fork"}, []string{"sessions/import/name/fork"}},
		{[]string{"--import --force"}, []string{"sessions/import/name/force"}},
		{[]string{"--export"}, []string{"sessions/export/name/copy"}},
		{[]string{"--export --fork"}, []string{"sessions/export/name/fork"}},
		{[]string{"--export --force"}, []string{"sessions/export/name/force"}},

		{[]string{"--status"}, []string{"server/status"}},
		{[]string{"--start"}, []string{"server/start"}},
		{[]string{"--stop"}, []string{"server/stop"}},
		{[]string{"--restart"}, []string{"server/restart"}},
		{[]string{"--logs"}, []string{"server/logs"}},
		{[]string{"--doctor"}, []string{"server/doctor"}},
		{[]string{"--version"}, []string{"server/version"}},
		{[]string{"--update --check"}, []string{"server/check_update"}},
		{[]string{"--update"}, []string{"server/update"}},
	}
	return f
}

// cliEntries are the commands and their verbs, as "--peak tz".
// needsVerb are the commands that do nothing without one.
var needsVerb = map[string]bool{"--profile": true, "--config": true}

func cliEntries() []string {
	var out []string
	for _, c := range commands() {
		if c.hidden {
			continue
		}
		if !needsVerb[c.name] {
			out = append(out, c.name)
		}
		for _, v := range c.verbs {
			out = append(out, c.name+" "+v)
		}
	}
	sort.Strings(out)
	return out
}

// parityConfig has one of everything the screens list: every kind of provider,
// profiles that set every key, peak hours.
const parityConfig = `listen.port: 60093

providers: [
    {
        name: codex
        type: codex
    }
    {
        name: zai
        type: anthropic_compat
        base_url: https://api.z.ai/api/anthropic
        api_key_env: ZAI_KEY
        peak_tz: +08:00
        peak: [
            {
                days: mon-fri
                from: 14:00
                to: 18:00
            }
        ]
    }
    {
        name: oai
        type: codex_api
        base_url: https://oai.example/v1
    }
    {
        name: work
        type: claude
    }
    {
        name: api
        type: claude_api
    }
]

profiles: [
    {
        name: codex
        provider: codex
    }
    {
        name: oai
        provider: oai
    }
    {
        name: work
        provider: work
    }
    {
        name: api
        provider: api
    }
    {
        name: zai
        provider: zai
        default_model: glm-old
        context_window: 1000000
        compact_percent: 80
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

// pick selects the item at index at of the menu on top.
func (h *tui) pick(at int) {
	h.t.Helper()
	m, ok := h.a.top().(*menu)
	if !ok {
		h.t.Fatalf("the top screen is not a menu:\n%s", h.view())
	}
	for ; m.cursor < at; m.cursor++ {
	}
	for ; m.cursor > at; m.cursor-- {
	}
	h.press("enter")
}

// crawlTUI opens every item of every screen, each on a fresh interface, and
// returns the id paths found with what the item does: "menu" (opens a list),
// "input", "confirm" or "action". A path met with several kinds (a list that is
// empty for one provider) keeps the first of menu, input, confirm, action.
func crawlTUI(t *testing.T) map[string]string {
	t.Helper()
	rank := map[string]int{"menu": 0, "input": 1, "confirm": 2, "view": 3, "action": 4}
	found := map[string]string{}
	var walk func(steps []int, ids []string)
	walk = func(steps []int, ids []string) {
		fresh := func() *tui {
			h := newTUI(t, parityConfig)
			for _, i := range steps {
				h.pick(i)
			}
			return h
		}
		h := fresh()
		m, ok := h.a.top().(*menu)
		if !ok {
			return
		}
		for i, it := range m.items(h.a) {
			if it.id == "" {
				t.Errorf("the item %q on %q has no id", it.label, strings.Join(ids, "/"))
				continue
			}
			path := append(append([]string{}, ids...), it.id)
			h2 := fresh()
			before := len(h2.a.stack)
			h2.pick(i)
			kind := "action"
			if len(h2.a.stack) > before {
				switch h2.a.top().(type) {
				case *menu:
					kind = "menu"
				case *input:
					kind = "input"
				case *confirmScreen:
					kind = "confirm"
				case *viewScreen:
					kind = "view"
				}
			}
			key := strings.Join(path, "/")
			if old, seen := found[key]; !seen || rank[kind] < rank[old] {
				found[key] = kind
			}
			if kind == "menu" {
				walk(append(append([]int{}, steps...), i), path)
			}
		}
	}
	walk(nil, nil)
	return found
}

func isNav(path string) bool {
	return strings.HasPrefix(path[strings.LastIndex(path, "/")+1:], "nav:")
}

func TestParityEveryCommandHasAnInterfacePath(t *testing.T) {
	mapped := map[string]bool{}
	for _, f := range features() {
		for _, e := range f.cli {
			mapped[e] = true
		}
	}
	entries := cliEntries()
	have := map[string]bool{}
	for _, e := range entries {
		have[e] = true
		if reason, ok := cliOnly[e]; ok {
			if reason == "" {
				t.Errorf("%s is left out of the interface without a reason", e)
			}
			if mapped[e] {
				t.Errorf("%s is both in a feature and in cliOnly", e)
			}
			continue
		}
		if !mapped[e] {
			t.Errorf("the command %q has no feature: give the interface a way to do it, and add it to features()", e)
		}
	}
	for e := range mapped {
		if !have[e] {
			t.Errorf("features() names %q, which is not a command or a verb (commands() and command.verbs)", e)
		}
	}
	for e := range cliOnly {
		if !have[e] {
			t.Errorf("cliOnly names %q, which is not a command", e)
		}
	}
	for _, f := range features() {
		if len(f.cli) == 0 || len(f.tui) == 0 {
			t.Errorf("feature %v / %v lacks a side", f.cli, f.tui)
		}
	}
}

func TestParityEveryInterfaceItemHasACommand(t *testing.T) {
	crawled := crawlTUI(t)
	mapped := map[string]bool{}
	for _, f := range features() {
		for _, p := range f.tui {
			mapped[p] = true
			if _, ok := crawled[p]; !ok {
				t.Errorf("features() names the screen item %q, which the interface does not have", p)
			}
		}
	}
	for path, kind := range crawled {
		switch {
		case isNav(path), mapped[path], kind == "menu":
			// Moving around, or a list that leads to items checked on their own.
		default:
			t.Errorf("the screen item %q (%s) does nothing a command does: add the command and its feature", path, kind)
		}
	}
	if len(crawled) < 60 {
		t.Errorf("the crawl found only %d items: it does not reach the screens", len(crawled))
	}
}

// Help and completion say what the table of verbs says.
func TestParityHelpAndCompletionNameEveryVerb(t *testing.T) {
	for _, c := range commands() {
		for _, v := range c.verbs {
			text := commandHelp[c.name]
			for _, word := range strings.Fields(v) {
				if !strings.Contains(text, word) {
					t.Errorf("the help of %s does not mention %q (verb %q)", c.name, word, v)
				}
			}
			first := strings.Fields(v)[0]
			if c.name == "--connections" && first == "proxy" {
				continue // its second word is completed after the value; the first is below
			}
			words := []string{"ccgp", c.name, ""}
			if c.name == "--peak" {
				words = []string{"ccgp", c.name, "zai", ""} // the verbs come after a provider
			}
			got := completeWords(words, testNames)
			if !contains(got, first) {
				t.Errorf("completing %s does not offer %q (verb %q): %q", c.name, first, v, got)
			}
		}
	}
}

// ---- the same result from both sides ----

// snapshot is what a run leaves: the config and the peak pauses.
func snapshot(t *testing.T, cfgPath, credsDir string) string {
	t.Helper()
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	pause, _ := os.ReadFile(filepath.Join(credsDir, "peak-pause.ktav"))
	return string(cfg) + "\n--- pauses ---\n" + string(pause)
}

// runCommand runs a command line in this process, as main does, with its
// output out of the way. The peak command gets the fixed clock of the tests.
func runCommand(t *testing.T, args []string, pauses *config.PeakPauses) error {
	t.Helper()
	if args[0] == "--peak" {
		return peak(&bytes.Buffer{}, peakNow, pauses, args[1:])
	}
	c, ok := findCommand(args[0])
	if !ok {
		t.Fatalf("%q is not a command", args[0])
	}
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = out
	defer func() { os.Stdout = saved; _ = out.Close() }()
	return c.run(args[1:])
}

type editRun struct {
	entries []string
	cli     []string
	prime   [][]string // peak commands run first on both sides
	tui     func(h *tui)
}

func replaceAndEnter(h *tui, text string) {
	h.replace(text)
	h.press("enter")
}

func editRuns() []editRun {
	field := func(path []string, text string) func(h *tui) {
		return func(h *tui) { h.open(path...); replaceAndEnter(h, text) }
	}
	emptied := func(path ...string) func(h *tui) {
		return func(h *tui) { h.open(path...); h.press("ctrl+u", "enter") }
	}
	zai := func(tail ...string) []string { return append([]string{"profiles", "profile:zai"}, tail...) }
	zaiProvider := func(tail ...string) []string { return append([]string{"providers", "provider:zai"}, tail...) }
	peakZai := func(tail ...string) []string { return append([]string{"peak", "provider:zai"}, tail...) }
	confirm := func(path ...string) func(h *tui) {
		return func(h *tui) { h.open(path...); h.press("y") }
	}
	addFlow := func(path []string, name string, steps ...string) func(h *tui) {
		return func(h *tui) {
			h.open(path...)
			for _, s := range steps {
				h.typ(s)
				h.press("enter")
			}
			replaceAndEnter(h, name) // the name
			h.press("enter")         // no catch-all model
		}
	}
	return []editRun{
		{[]string{"--profile set default_model"}, []string{"--profile", "set", "zai", "default_model", "glm-new"}, nil, field(zai("default_model"), "glm-new")},
		{[]string{"--profile unset default_model"}, []string{"--profile", "unset", "zai", "default_model"}, nil, emptied(zai("default_model")...)},
		{[]string{"--profile set context_window"}, []string{"--profile", "set", "zai", "context_window", "500000"}, nil, field(zai("context_window"), "500000")},
		{[]string{"--profile unset context_window"}, []string{"--profile", "unset", "zai", "context_window"}, nil, emptied(zai("context_window")...)},
		{[]string{"--profile set compact_percent"}, []string{"--profile", "set", "zai", "compact_percent", "70"}, nil, field(zai("compact_percent"), "70")},
		{[]string{"--profile unset compact_percent"}, []string{"--profile", "unset", "zai", "compact_percent"}, nil, emptied(zai("compact_percent")...)},
		{[]string{"--profile set provider"}, []string{"--profile", "set", "zai2", "provider", "oai"}, nil,
			func(h *tui) { h.open("profiles", "profile:zai2", "move", "provider:oai") }},
		{[]string{"--connections proxy profile"}, []string{"--connections", "proxy", "socks5://127.0.0.1:1080", "profile", "zai"}, nil, field(zai("proxy"), "socks5://127.0.0.1:1080")},
		{[]string{"--connections proxy profile"}, []string{"--connections", "proxy", "none", "profile", "zai"}, nil, field(zai("proxy"), "none")},
		{[]string{"--connections proxy provider"}, []string{"--connections", "proxy", "http://corp:3128", "provider", "zai"}, nil, field(zaiProvider("proxy"), "http://corp:3128")},
		{[]string{"--connections proxy provider"}, []string{"--connections", "proxy", "none", "provider", "oai"}, nil, field([]string{"providers", "provider:oai", "proxy"}, "none")},
		{[]string{"--connections proxy"}, []string{"--connections", "proxy", "socks5://127.0.0.1:1080"}, nil, field([]string{"config", "proxy"}, "socks5://127.0.0.1:1080")},
		{[]string{"--connections active"}, []string{"--connections", "active", "5"}, nil, field([]string{"config", "active"}, "5")},
		{[]string{"--connections total"}, []string{"--connections", "total", "12"}, nil, field([]string{"config", "total"}, "12")},

		{[]string{"--provider set base_url"}, []string{"--provider", "set", "oai", "base_url", "https://other.example/v1"}, nil, field([]string{"providers", "provider:oai", "base_url"}, "https://other.example/v1")},
		{[]string{"--provider unset base_url"}, []string{"--provider", "unset", "oai", "base_url"}, nil, emptied("providers", "provider:oai", "base_url")},
		{[]string{"--provider set api_key_env"}, []string{"--provider", "set", "oai", "api_key_env", "OAI_KEY"}, nil, field([]string{"providers", "provider:oai", "api_key_env"}, "OAI_KEY")},
		{[]string{"--provider unset api_key_env"}, []string{"--provider", "unset", "zai", "api_key_env"}, nil, emptied(zaiProvider("api_key_env")...)},
		{[]string{"--provider set base_url"}, []string{"--provider", "set", "api", "base_url", "https://gw.example"}, nil, field([]string{"profiles", "profile:api", "base_url"}, "https://gw.example")},
		{[]string{"--provider set base_url"}, []string{"--provider", "set", "api", "base_url", "https://gw.example"}, nil, field([]string{"providers", "provider:api", "base_url"}, "https://gw.example")},

		{[]string{"--peak set"}, []string{"--peak", "zai", "set", "sat,sun", "10:00-12:00"}, nil, field(peakZai("set"), "sat,sun 10:00-12:00")},
		{[]string{"--peak add"}, []string{"--peak", "zai", "add", "sat,sun", "10:00-12:00"}, nil, func(h *tui) { h.open(peakZai("add")...); h.typ("sat,sun 10:00-12:00"); h.press("enter") }},
		{[]string{"--peak rm"}, []string{"--peak", "zai", "rm", "1"}, nil, func(h *tui) { h.open(peakZai("rm", "window:mon-fri")...) }},
		{[]string{"--peak tz"}, []string{"--peak", "zai", "tz", "Asia/Shanghai"}, nil, field(peakZai("tz"), "Asia/Shanghai")},
		{[]string{"--peak clear"}, []string{"--peak", "zai", "clear"}, nil, confirm(peakZai("clear")...)},
		{[]string{"--peak pause"}, []string{"--peak", "zai", "pause", "2h"}, nil, func(h *tui) { h.open(peakZai("pause")...); h.typ("2h"); h.press("enter") }},
		{[]string{"--peak resume"}, []string{"--peak", "zai", "resume"}, [][]string{{"zai", "pause", "2h"}}, func(h *tui) { h.open(peakZai("resume")...) }},
		{[]string{"--peak tz"}, []string{"--peak", "zai", "tz", "UTC"}, nil, field(zaiProvider("peak", "tz"), "UTC")},

		{[]string{"--profile add --like"}, []string{"--profile", "add", "neo", "--like", "codex"}, nil, addFlow([]string{"profiles", "add", "add_like", "name:codex"}, "neo")},
		{[]string{"--profile add --provider"}, []string{"--profile", "add", "neo", "--provider", "zai"}, nil, addFlow([]string{"profiles", "add", "add_provider", "name:zai"}, "neo")},
		{[]string{"--profile add --type"}, []string{"--profile", "add", "neo", "--type", "anthropic_compat", "--base-url", "https://x.example/anthropic"}, nil,
			addFlow([]string{"profiles", "add", "add_type", "kind_anthropic_compat"}, "neo", "https://x.example/anthropic")},
		{[]string{"--profile add --type"}, []string{"--profile", "add", "neo", "--type", "codex_api", "--base-url", "https://x.example/v1"}, nil,
			addFlow([]string{"providers", "add", "kind_codex_api"}, "neo", "https://x.example/v1")},
		{[]string{"--profile add --type"}, []string{"--profile", "add", "neo", "--type", "claude_api", "--base-url", "https://x.example"}, nil,
			func(h *tui) {
				h.open("profiles", "add", "add_type", "kind_claude_api")
				h.typ("https://x.example")
				h.press("enter")
				replaceAndEnter(h, "neo") // no model step: a Claude profile has no rules
			}},
		{[]string{"--profile rm"}, []string{"--profile", "rm", "zai2", "--yes"}, nil, func(h *tui) { h.open("profiles", "profile:zai2", "rm", "rm_keep") }},
		{[]string{"--profile rm"}, []string{"--profile", "rm", "zai2", "--yes", "--purge"}, nil, func(h *tui) { h.open("profiles", "profile:zai2", "rm", "rm_purge") }},
	}
}

func TestParityEditsLeaveTheSameFile(t *testing.T) {
	t.Cleanup(func() { proxydial.SetDefault(nil) }) // loading a config with a global proxy sets the default one
	for _, r := range editRuns() {
		name := strings.Join(r.cli, " ")
		t.Run(name, func(t *testing.T) {
			// The interface.
			h := newTUI(t, parityConfig)
			for _, p := range r.prime {
				if err := peak(&bytes.Buffer{}, peakNow, h.a.pauses, p); err != nil {
					t.Fatalf("prime %v: %v", p, err)
				}
			}
			r.tui(h)
			if h.a.errMsg != "" {
				t.Fatalf("the interface refused: %s\n%s", h.a.errMsg, h.view())
			}
			if in, ok := h.a.top().(*input); ok && in.err != "" {
				t.Fatalf("the interface refused: %s", in.err)
			}
			viaTUI := snapshot(t, h.cfgPath, h.store.Dir)

			// The command line, on the same config in another place.
			root := t.TempDir()
			path := filepath.Join(root, "config.ktav")
			if err := os.WriteFile(path, []byte(parityConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(config.ConfigFileEnv, path)
			t.Setenv("CCGP_HOME", filepath.Join(root, "home"))
			pauses := config.NewPeakPauses(filepath.Join(root, "creds"))
			for _, p := range r.prime {
				if err := peak(&bytes.Buffer{}, peakNow, pauses, p); err != nil {
					t.Fatalf("prime %v: %v", p, err)
				}
			}
			if err := runCommand(t, r.cli, pauses); err != nil {
				t.Fatalf("the command failed: %v", err)
			}
			viaCLI := snapshot(t, path, filepath.Join(root, "creds"))

			if viaTUI != viaCLI {
				t.Errorf("the interface and `ccgp %s` leave different files.\ninterface:\n%s\ncommand:\n%s", name, viaTUI, viaCLI)
			}
			if len(r.prime) == 0 && viaCLI == snapshot0(t, root) {
				t.Errorf("`ccgp %s` changed nothing: the run proves nothing", name)
			}
		})
	}
}

// snapshot0 is the snapshot of the fixture before any run.
func snapshot0(t *testing.T, root string) string {
	t.Helper()
	p := filepath.Join(root, "initial.ktav")
	if err := os.WriteFile(p, []byte(parityConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return snapshot(t, p, filepath.Join(root, "no-creds"))
}

func TestParityEditRunsCoverTheEditingEntries(t *testing.T) {
	covered := map[string]bool{}
	for _, r := range editRuns() {
		for _, e := range r.entries {
			covered[e] = true
		}
	}
	for _, r := range cmdRuns() {
		for _, e := range r.entries {
			covered[e] = true
		}
	}
	for _, e := range showEntries {
		covered[e] = true
	}
	for _, f := range features() {
		for _, e := range f.cli {
			if !covered[e] {
				t.Errorf("no run proves that the interface does what %q does", e)
			}
		}
	}
}

// ---- the same command from both sides ----

// cmdRun is a feature that runs a command: the interface must start exactly the
// command line given, and that line must be one the command line takes.
type cmdRun struct {
	entries  []string
	cli      []string
	terminal bool
	tui      func(h *tui)
}

func cmdRuns() []cmdRun {
	press := func(path []string, keys ...string) func(h *tui) {
		return func(h *tui) { h.open(path...); h.press(keys...) }
	}
	typed := func(path []string, text string, keys ...string) func(h *tui) {
		return func(h *tui) { h.open(path...); h.typ(text); h.press("enter"); h.press(keys...) }
	}
	return []cmdRun{
		{[]string{"--status"}, []string{"--status"}, false, press([]string{"server", "status"})},
		{[]string{"--start"}, []string{"--start"}, false, press([]string{"server", "start"})},
		{[]string{"--stop"}, []string{"--stop"}, false, press([]string{"server", "stop"})},
		{[]string{"--restart"}, []string{"--restart"}, false, press([]string{"server", "restart"})},
		{[]string{"--logs"}, []string{"--logs"}, false, press([]string{"server", "logs"})},
		{[]string{"--doctor"}, []string{"--doctor"}, false, press([]string{"server", "doctor"})},
		{[]string{"--version"}, []string{"--version"}, false, press([]string{"server", "version"})},
		{[]string{"--update --check"}, []string{"--update", "--check"}, false, press([]string{"server", "check_update"})},
		{[]string{"--update"}, []string{"--update"}, true, press([]string{"server", "update"}, "y")},

		{[]string{"--models"}, []string{"--models"}, false, press([]string{"models", "show"})},
		{[]string{"--models"}, []string{"--models", "zai"}, false, press([]string{"models", "profile:zai", "show"})},
		{[]string{"--models apply"}, []string{"--models", "apply"}, false, press([]string{"models", "apply"}, "y")},
		{[]string{"--models apply"}, []string{"--models", "apply", "zai"}, false, press([]string{"models", "profile:zai", "apply"}, "y")},
		{[]string{"--which"}, []string{"--which", "zai", "claude-opus-4-1"}, false, typed([]string{"models", "profile:zai", "which"}, "claude-opus-4-1")},
		{[]string{"--config reset"}, []string{"--config", "reset"}, false, press([]string{"config", "reset"}, "y")},

		{[]string{"--login"}, []string{"--login", "codex"}, true, press([]string{"providers", "provider:codex", "login"})},
		{[]string{"--login --device"}, []string{"--login", "codex", "--device"}, true, press([]string{"providers", "provider:codex", "login_device"})},
		{[]string{"--login"}, []string{"--login", "zai"}, true, press([]string{"providers", "provider:zai", "login"})},
		{[]string{"--login"}, []string{"--login", "api"}, true, press([]string{"profiles", "profile:api", "login"})},
		{[]string{"--logout"}, []string{"--logout", "codex", "--yes"}, false, press([]string{"providers", "provider:codex", "logout"}, "y")},

		{[]string{"--import"}, []string{"--import", "zai"}, false, typed([]string{"sessions", "import", "name:zai", "copy"}, "")},
		{[]string{"--import"}, []string{"--import", "zai", "3"}, false, typed([]string{"sessions", "import", "name:zai", "copy"}, "3")},
		{[]string{"--import --fork"}, []string{"--import", "zai", "--fork"}, false, typed([]string{"sessions", "import", "name:zai", "fork"}, "")},
		{[]string{"--import --force"}, []string{"--import", "zai", "2", "--force"}, false, typed([]string{"sessions", "import", "name:zai", "force"}, "2")},
		{[]string{"--export"}, []string{"--export", "codex", "2"}, false, typed([]string{"sessions", "export", "name:codex", "copy"}, "2")},
		{[]string{"--export --fork"}, []string{"--export", "codex", "2", "--fork"}, false, typed([]string{"sessions", "export", "name:codex", "fork"}, "2")},
		{[]string{"--export --force"}, []string{"--export", "codex", "--force"}, false, typed([]string{"sessions", "export", "name:codex", "force"}, "")},
		{[]string{"--send"}, []string{"--send", "codex", "zai"}, false, typed([]string{"sessions", "send", "name:codex", "name:zai", "copy"}, "")},
		{[]string{"--send --fork"}, []string{"--send", "codex", "zai", "1", "--fork"}, false, typed([]string{"sessions", "send", "name:codex", "name:zai", "fork"}, "1")},
		{[]string{"--send --force"}, []string{"--send", "codex", "zai", "--force"}, false, typed([]string{"sessions", "send", "name:codex", "name:zai", "force"}, "")},
	}
}

func TestParityTheInterfaceStartsTheCommand(t *testing.T) {
	for _, r := range cmdRuns() {
		name := strings.Join(r.cli, " ")
		t.Run(name, func(t *testing.T) {
			if _, ok := findCommand(r.cli[0]); !ok {
				t.Fatalf("%q is not a command", r.cli[0])
			}
			h := newTUI(t, parityConfig)
			r.tui(h)
			want := []ran{{args: r.cli, terminal: r.terminal}}
			if !reflect.DeepEqual(h.ran, want) {
				t.Errorf("the interface started %+v, want %+v", h.ran, want)
			}
			if h.a.errMsg != "" {
				t.Errorf("error: %s", h.a.errMsg)
			}
		})
	}
}

// What the interface asks before it acts: nothing starts on a no.
func TestParityDestructiveItemsAsk(t *testing.T) {
	for _, path := range [][]string{
		{"server", "update"}, {"models", "apply"}, {"models", "profile:zai", "apply"},
		{"config", "reset"}, {"providers", "provider:codex", "logout"},
	} {
		h := newTUI(t, parityConfig)
		h.open(path...)
		if _, ok := h.a.top().(*confirmScreen); !ok {
			t.Errorf("%v does not ask first:\n%s", path, h.view())
			continue
		}
		h.press("n")
		if len(h.ran) != 0 {
			t.Errorf("%v started %+v after a no", path, h.ran)
		}
	}
}

// ---- what is shown ----

// showEntries are the entries that only show something: the interface has the
// same on a screen.
var showEntries = []string{"--profile list", "--provider", "--connections", "--peak"}

func TestParityShowScreensShowWhatTheCommandsPrint(t *testing.T) {
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	h := newTUI(t, parityConfig)

	// --provider NAME: the same text.
	var buf bytes.Buffer
	if err := provider(&buf, h.a.cfg, h.store, h.a.pauses, peakNow, []string{"zai"}); err != nil {
		t.Fatal(err)
	}
	h.open("providers", "provider:zai")
	want := strings.TrimRight(buf.String(), "\n")
	if view := h.view(); !strings.Contains(spaces.ReplaceAllString(view, " "), spaces.ReplaceAllString(want, " ")) {
		t.Errorf("the provider screen lacks the text of `ccgp --provider zai`:\n%s\n--- screen ---\n%s", want, view)
	}

	// --connections: the same text on the config screen.
	h = newTUI(t, parityConfig)
	buf.Reset()
	if err := connections(&buf, nil); err != nil {
		t.Fatal(err)
	}
	h.open("config")
	if view := spaces.ReplaceAllString(h.view(), " "); !strings.Contains(view, spaces.ReplaceAllString(strings.TrimRight(buf.String(), "\n"), " ")) {
		t.Errorf("the config screen lacks the text of `ccgp --connections`:\n%s\n--- screen ---\n%s", buf.String(), h.view())
	}

	// --peak zai: the same text.
	h = newTUI(t, parityConfig)
	buf.Reset()
	if err := showPeak(&buf, h.a.cfg, peakNow, h.a.pauses, "zai", false); err != nil {
		t.Fatal(err)
	}
	h.open("peak", "provider:zai")
	if view := spaces.ReplaceAllString(h.view(), " "); !strings.Contains(view, spaces.ReplaceAllString(strings.TrimRight(buf.String(), "\n"), " ")) {
		t.Errorf("the peak screen lacks the text of `ccgp --peak zai`:\n%s\n--- screen ---\n%s", buf.String(), h.view())
	}

	// --profile list: every profile of the list is a row.
	h = newTUI(t, parityConfig)
	h.open("profiles")
	for _, r := range buildProfileRows(h.a.cfg, h.store, h.claudeBase) {
		if !strings.Contains(h.view(), r.Name) || !strings.Contains(h.view(), r.Type) {
			t.Errorf("the profile list lacks %s (%s):\n%s", r.Name, r.Type, h.view())
		}
	}
}
