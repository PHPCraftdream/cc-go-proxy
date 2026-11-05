package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

// Profiles, sorted: codex, work, zai. Providers, sorted: codex, work, zai.
const uiConfig = `listen.port: 60093

providers: [
    {
        name: codex
        type: codex
    }
    {
        name: zai
        type: anthropic_compat
        base_url: https://api.z.ai/api/anthropic
    }
    {
        name: work
        type: claude
    }
]

profiles: [
    {
        name: codex
        provider: codex
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
        name: work
        provider: work
    }
]
`

// tui plays the interface: keys go to Update, the screen is what View draws.
type tui struct {
	t          *testing.T
	a          *app
	cfgPath    string
	claudeBase string
	store      creds.Store
	ran        []ran
}

func newTUI(t *testing.T, cfgText string) *tui {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CCGP_HOME", filepath.Join(root, "home"))
	path := filepath.Join(root, "config.ktav")
	if err := os.WriteFile(path, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, path)
	h := &tui{t: t, cfgPath: path, claudeBase: filepath.Join(root, "claude"), store: creds.Store{Dir: filepath.Join(root, "creds")}}
	a, err := newApp(h.store, h.claudeBase, func() time.Time { return peakNow })
	if err != nil {
		t.Fatal(err)
	}
	a.run = h.fakeRun
	h.a = a
	return h
}

// ran is a command the interface started: its arguments, and whether it had
// the terminal.
type ran struct {
	args     []string
	terminal bool
}

// fakeRun stands for runSelf: it records the command and answers at once.
func (h *tui) fakeRun(title string, args []string, terminal bool) tea.Cmd {
	h.ran = append(h.ran, ran{args: args, terminal: terminal})
	return func() tea.Msg {
		if terminal {
			return termDoneMsg{title: title}
		}
		return outputMsg{title: title, text: "output of ccgp " + strings.Join(args, " ")}
	}
}

// settle runs the command a key returned and gives its answer to the model,
// as the program does.
func (h *tui) settle(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch m := cmd().(type) {
	case outputMsg, termDoneMsg:
		h.a.Update(m)
	}
}

// open opens items by their ids: "profile:zai" is the item of the id profile
// whose label starts with zai. It fails when an item is not there.
func (h *tui) open(path ...string) {
	h.t.Helper()
	for _, step := range path {
		id, name, _ := strings.Cut(step, ":")
		m, ok := h.a.top().(*menu)
		if !ok {
			h.t.Fatalf("the top screen is not a menu at %q:\n%s", step, h.view())
		}
		at := -1
		for i, it := range m.items(h.a) {
			if it.id == id && (name == "" || strings.Fields(it.label)[0] == name) {
				at = i
				break
			}
		}
		if at < 0 {
			h.t.Fatalf("no item %q on the screen:\n%s", step, h.view())
		}
		h.pick(at)
	}
}

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
	spaces = regexp.MustCompile(` +`)
)

var namedKeys = map[string]tea.KeyType{
	"enter": tea.KeyEnter, "esc": tea.KeyEsc, "up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft,
	"right": tea.KeyRight, "backspace": tea.KeyBackspace, "delete": tea.KeyDelete, "home": tea.KeyHome,
	"end": tea.KeyEnd, "ctrl+c": tea.KeyCtrlC, "ctrl+u": tea.KeyCtrlU, "ctrl+k": tea.KeyCtrlK,
	"ctrl+w": tea.KeyCtrlW, "ctrl+a": tea.KeyCtrlA, "ctrl+e": tea.KeyCtrlE,
}

// press sends keys: a name from namedKeys, or a single character.
func (h *tui) press(keys ...string) {
	h.t.Helper()
	for _, k := range keys {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		if typ, ok := namedKeys[k]; ok {
			msg = tea.KeyMsg{Type: typ}
		} else if k == " " {
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		}
		_, cmd := h.a.Update(msg)
		h.settle(cmd)
	}
}

// typ types a text, a space as the key it is.
func (h *tui) typ(s string) {
	h.t.Helper()
	for _, r := range s {
		h.press(string(r))
	}
}

// replace clears the field and types s into it.
func (h *tui) replace(s string) {
	h.t.Helper()
	h.press("end", "ctrl+u")
	h.typ(s)
}

func (h *tui) view() string { return ansiRe.ReplaceAllString(h.a.View(), "") }

func (h *tui) config() *config.Config {
	h.t.Helper()
	cfg, err := config.Load()
	if err != nil {
		h.t.Fatalf("the config does not load: %v\n%s", err, h.text())
	}
	return cfg
}

func (h *tui) text() string {
	b, err := os.ReadFile(h.cfgPath)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

// has checks that the screen holds each text, ignoring runs of spaces.
func (h *tui) has(wants ...string) {
	h.t.Helper()
	view := spaces.ReplaceAllString(h.view(), " ")
	for _, w := range wants {
		if !strings.Contains(view, spaces.ReplaceAllString(w, " ")) {
			h.t.Errorf("the screen lacks %q:\n%s", w, h.view())
		}
	}
}

func (h *tui) lacks(unwanted ...string) {
	h.t.Helper()
	view := spaces.ReplaceAllString(h.view(), " ")
	for _, w := range unwanted {
		if strings.Contains(view, spaces.ReplaceAllString(w, " ")) {
			h.t.Errorf("the screen has %q:\n%s", w, h.view())
		}
	}
}

func (h *tui) input() *input {
	h.t.Helper()
	in, ok := h.a.top().(*input)
	if !ok {
		h.t.Fatalf("the top screen is not an input:\n%s", h.view())
	}
	return in
}

func TestTUIMainMenu(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.has("ccgp", h.cfgPath, "1 Profiles (3)", "2 Providers (3)", "3 Peak hours", "4 Config and connections", "5 Models", "6 Sessions", "7 Server", "8 Quit", "enter or number select", "esc back")
	h.press("down", "down", "enter")
	h.has("Peak hours: which provider?")
	h = newTUI(t, uiConfig)
	h.press("up", "enter") // up from the first wraps to Quit
	if !h.a.quit {
		t.Error("the last item did not quit")
	}

	for _, k := range []string{"8", "esc", "q", "ctrl+c"} {
		h := newTUI(t, uiConfig)
		h.press(k)
		if !h.a.quit {
			t.Errorf("%q did not quit from the first screen", k)
		}
	}
	h = newTUI(t, uiConfig)
	h.press("1", "ctrl+c")
	if !h.a.quit {
		t.Error("ctrl+c did not quit from a deeper screen")
	}
	h = newTUI(t, uiConfig)
	h.press("1", "esc")
	if h.a.quit || len(h.a.stack) != 1 {
		t.Error("esc from a list should go back, not quit")
	}
	if h.text() != uiConfig {
		t.Error("moving about changed the config")
	}
}

func TestTUIShowsAProfile(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("1")
	h.has("Profiles", "codex codex codex", "work work claude", "zai zai anthropic_compat", "+ add a profile")
	h.press("3")
	h.has("Profile zai", "provider zai (anthropic_compat)", "proxy global (environment)", "models 1 rule(s)",
		"* -> glm-5.3-flash (1000000)", "1 default_model -", "2 context_window -", "3 compact_percent -", "4 proxy -",
		"5 move to another provider…", "6 remove the profile")

	h = newTUI(t, uiConfig)
	h.press("1", "2")
	h.has("Profile work", "provider work (claude)", "the real Claude Code: nothing goes through ccgp", "1 remove the profile")
	h.lacks("default_model", "move to another provider")
}

func TestTUIEditsProfileSettings(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("1", "3", "1")
	h.has("Profile zai", "default_model:", "the backend model", "enter accept")
	h.typ("glm-5.3")
	h.press("enter")
	h.has("Profile zai", "config.ktav updated, backup", "1 default_model glm-5.3")
	h.press("2")
	h.typ("500000")
	h.press("enter")
	h.press("3")
	h.typ("70")
	h.press("enter")
	pf := h.config().Profiles["zai"]
	if pf.DefaultModel != "glm-5.3" || pf.ContextWindow != 500000 || pf.CompactPercent != 70 || len(pf.Models) != 1 {
		t.Errorf("profile: %+v", pf)
	}

	// The field starts with the value; emptying it removes the setting.
	h.press("1")
	if got := string(h.input().text); got != "glm-5.3" {
		t.Errorf("the field starts with %q", got)
	}
	h.press("ctrl+u", "enter")
	if pf := h.config().Profiles["zai"]; pf.DefaultModel != "" {
		t.Errorf("default_model after emptying: %q", pf.DefaultModel)
	}
	h.has("1 default_model -")

	// Esc leaves the field and the file alone.
	before := h.text()
	h.press("1")
	h.typ("zzz")
	h.press("esc")
	h.has("Profile zai")
	if h.text() != before {
		t.Error("esc changed the config")
	}
}

func TestTUIRefusesWrongValuesInTheField(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("1", "3", "3")
	h.typ("150")
	h.press("enter")
	in := h.input() // still the field
	if in.err == "" {
		t.Fatal("no error for a compact percent of 150")
	}
	h.has("error:", "compact_percent: 150")
	h.press("ctrl+u")
	h.typ("abc")
	h.press("enter")
	h.has("error:")
	h.press("esc", "2")
	h.typ("abc")
	h.press("enter")
	h.has("error:")
	if h.text() != uiConfig {
		t.Errorf("a refused value changed the config:\n%s", h.text())
	}
}

func TestTUIEditorKeys(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("1", "3", "1")
	h.typ("abc")
	h.press("left", "left")
	h.typ("X")
	if got := string(h.input().text); got != "aXbc" {
		t.Errorf("insert: %q", got)
	}
	h.press("home", "delete")
	if got := string(h.input().text); got != "Xbc" {
		t.Errorf("home, delete: %q", got)
	}
	h.press("end", "backspace")
	if got := string(h.input().text); got != "Xb" {
		t.Errorf("end, backspace: %q", got)
	}
	h.press("left", "ctrl+k")
	if got := string(h.input().text); got != "X" {
		t.Errorf("ctrl+k: %q", got)
	}
	h.press("end", "ctrl+u")
	h.typ("one two  three")
	h.press("ctrl+w")
	if got := string(h.input().text); got != "one two  " {
		t.Errorf("ctrl+w: %q", got)
	}
	h.press("ctrl+w")
	if got := string(h.input().text); got != "one " {
		t.Errorf("ctrl+w again: %q", got)
	}
	h.press("ctrl+a", "ctrl+e", "right", "left", "left", "left", "left", "left", "left", "backspace")
	if got := string(h.input().text); got != "one " {
		t.Errorf("moving past the ends changed the text: %q", got)
	}
}

func TestTUIProfileProxy(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("1", "3", "4")
	h.typ("socks5://127.0.0.1:1080")
	h.press("enter")
	h.has("proxy socks5://127.0.0.1:1080 (its own)", "4 proxy socks5://127.0.0.1:1080")
	if pf := h.config().Profiles["zai"]; pf.Proxy != "socks5://127.0.0.1:1080" {
		t.Errorf("proxy = %q", pf.Proxy)
	}
	h.press("4")
	h.replace("none")
	h.press("enter")
	h.has("proxy none (its own)")
	h.press("4")
	h.press("ctrl+u", "enter") // empty: the global proxy again
	h.has("proxy global (")
	if pf := h.config().Profiles["zai"]; pf.Proxy != "" {
		t.Errorf("proxy after emptying = %q", pf.Proxy)
	}
	before := h.text()
	h.press("4")
	h.typ("ftp://nope")
	h.press("enter")
	h.has("error:")
	h.press("esc")
	if h.text() != before {
		t.Error("a wrong proxy changed the config")
	}
}

func TestTUIMovesAProfile(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("1", "1", "5")
	h.has("Move profile codex to", "1 zai anthropic_compat")
	h.lacks("work claude", "codex codex")
	h.press("1")
	h.has("Profile codex", "provider zai (anthropic_compat)", "config.ktav updated")
	if pf := h.config().Profiles["codex"]; pf.Provider != "zai" {
		t.Errorf("provider = %q", pf.Provider)
	}

	h = newTUI(t, uiConfig)
	h.press("1", "1", "5", "esc")
	h.has("Profile codex")
	if h.text() != uiConfig {
		t.Error("a cancelled move changed the config")
	}
}

func TestTUIRemovesAProfile(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("1", "1", "6")
	h.has("Remove profile codex", "1 remove it; keep its Claude dir and logins", "2 remove it and delete", "3 cancel")
	h.press("1")
	h.has("Profiles", `removed profile "codex"`)
	h.lacks("codex codex")
	cfg := h.config()
	if _, ok := cfg.Profiles["codex"]; ok {
		t.Error("profile codex is still there")
	}
	if _, ok := cfg.Providers["codex"]; ok {
		t.Error("provider codex stayed although no profile uses it")
	}

	h = newTUI(t, uiConfig)
	h.press("1", "1", "r", "3")
	h.has("Profile codex")
	if h.text() != uiConfig {
		t.Error("a cancelled removal changed the config")
	}

	// A Claude profile has nothing but removal.
	h = newTUI(t, uiConfig)
	h.press("1", "2", "1", "1")
	h.has("Profiles", `removed profile "work"`)
	if _, ok := h.config().Profiles["work"]; ok {
		t.Error("profile work is still there")
	}
}

func TestTUIPurgeDeletesTheClaudeDirAndLogin(t *testing.T) {
	h := newTUI(t, uiConfig)
	if err := os.MkdirAll(filepath.Join(h.claudeBase, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	login := h.store.OAuthPath("codex")
	if err := os.WriteFile(login, []byte("x: y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.press("1", "1", "6", "2")
	h.has("deleted the Claude dir", "deleted the credentials of provider")
	for _, p := range []string{filepath.Join(h.claudeBase, "codex"), login} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived --purge", p)
		}
	}
}

func TestTUIAddsAProfileLikeAnother(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("1", "a")
	h.has("Add a profile", "1 another account of the kind a profile has", "2 another profile on a login that exists", "3 a new backend")
	h.press("1")
	h.has("Copy which profile?", "1 codex", "2 work", "3 zai")
	h.press("3")
	h.has("profile name:", "zai2")
	h.press("enter")
	h.has("catch-all model:")
	h.press("enter") // no rule
	h.has("Profiles", `created profile "zai2" with its own provider "zai2"`, "zai2 zai2 anthropic_compat")
	cfg := h.config()
	if pf, pr := cfg.Profiles["zai2"], cfg.Providers["zai2"]; pf == nil || pr == nil || pf.Provider != "zai2" || pr.BaseURL != "https://api.z.ai/api/anthropic" {
		t.Errorf("zai2: %+v on %+v", pf, pr)
	}
	if len(h.a.stack) != 2 {
		t.Errorf("%d screens left on the stack, want the list over the main menu", len(h.a.stack))
	}

	// Like a Claude profile: no model step.
	h = newTUI(t, uiConfig)
	h.press("1", "a", "1", "2")
	h.has("profile name:", "work2")
	h.press("enter")
	h.has(`created Claude profile "work2"`)
	if pr := h.config().Providers["work2"]; pr == nil || pr.Type != config.ProviderTypeClaude {
		t.Errorf("work2: %+v", pr)
	}
}

func TestTUIAddsAProfileOnAnExistingLogin(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("1", "a", "2")
	h.has("On which provider?", "1 codex", "2 zai")
	h.lacks("3 work")
	h.press("2")
	h.replace("extra")
	h.press("enter")
	h.typ("my-model")
	h.press("enter")
	h.has("context window:", "272000")
	h.replace("123456")
	h.press("enter")
	h.has(`created profile "extra" sharing provider "zai"`)
	pf := h.config().Profiles["extra"]
	if pf == nil || pf.Provider != "zai" || len(pf.Models) != 1 || pf.Models[0].Model != "my-model" || pf.Models[0].ContextWindow != 123456 {
		t.Errorf("extra: %+v", pf)
	}

	// The window has a default.
	h = newTUI(t, uiConfig)
	h.press("1", "a", "2", "2")
	h.replace("extra")
	h.press("enter")
	h.typ("my-model")
	h.press("enter", "enter")
	if pf := h.config().Profiles["extra"]; pf == nil || pf.Models[0].ContextWindow != defaultCatchallWindow {
		t.Errorf("extra: %+v", pf)
	}
}

func TestTUIAddsABackendOfEachKind(t *testing.T) {
	// anthropic_compat needs a URL.
	h := newTUI(t, uiConfig)
	h.press("1", "a", "3")
	h.has("The kind of backend", "1 codex:", "2 codex_api:", "3 anthropic_compat:", "4 claude:")
	h.press("3")
	h.has("base URL:", "https://api.z.ai/api/anthropic")
	h.press("enter")
	h.has("error: a base URL is required for anthropic_compat")
	h.typ("https://example.com/anthropic")
	h.press("enter")
	h.has("profile name:", "custom")
	h.replace("raw")
	h.press("enter", "enter")
	if pr := h.config().Providers["raw"]; pr == nil || pr.Type != config.ProviderTypeAnthropicCompat || pr.BaseURL != "https://example.com/anthropic" {
		t.Errorf("raw: %+v", pr)
	}

	h = newTUI(t, uiConfig)
	h.press("1", "a", "3", "2")
	h.has("base URL:", "optional")
	h.press("enter") // none
	h.has("codex_api")
	h.press("enter")
	h.typ("gpt-x")
	h.press("enter", "enter")
	cfg := h.config()
	if pr := cfg.Providers["codex_api"]; pr == nil || pr.Type != config.ProviderTypeCodexAPI {
		t.Errorf("codex_api: %+v", pr)
	}
	if pf := cfg.Profiles["codex_api"]; pf == nil || len(pf.Models) != 1 || pf.Models[0].Model != "gpt-x" {
		t.Errorf("codex_api profile: %+v", pf)
	}

	h = newTUI(t, uiConfig)
	h.press("1", "a", "3", "1")
	h.has("profile name:", "codex2")
	h.press("enter", "enter")
	if pr := h.config().Providers["codex2"]; pr == nil || pr.Type != config.ProviderTypeCodex {
		t.Errorf("codex2: %+v", pr)
	}

	h = newTUI(t, uiConfig)
	h.press("1", "a", "3", "4")
	h.replace("mine")
	h.press("enter")
	h.has(`created Claude profile "mine"`)
	if pr := h.config().Providers["mine"]; pr == nil || pr.Type != config.ProviderTypeClaude {
		t.Errorf("mine: %+v", pr)
	}
}

func TestTUIAddStepsRefuseAndGoBack(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("1", "a", "2", "2")
	h.replace("zai")
	h.press("enter")
	h.has("error:", "already in use")
	h.replace("Bad Name")
	h.press("enter")
	h.has("error:")
	h.replace("extra")
	h.press("enter")
	h.typ("m")
	h.press("enter")
	h.replace("abc")
	h.press("enter")
	h.has("error: the context window must be a positive number")
	// An input that was accepted is gone, so esc goes back to the choice before it.
	h.press("esc")
	h.has("On which provider?")
	h.press("esc")
	h.has("Add a profile")
	h.press("esc")
	h.has("Profiles")
	if h.text() != uiConfig {
		t.Errorf("a cancelled wizard changed the config:\n%s", h.text())
	}
}

func TestTUIListsAndShowsProviders(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("2")
	h.has("Providers", "codex codex codex", "work claude work", "zai anthropic_compat zai", "+ add a provider")
	h.press("3")
	h.has("Provider zai", "type anthropic_compat", "base_url https://api.z.ai/api/anthropic", "api_key_env -",
		"peak hours none", "profiles zai", "proxy global (", "1 base_url https://api.z.ai/api/anthropic", "2 api_key_env -", "3 proxy -", "4 peak hours…")

	h = newTUI(t, uiConfig)
	h.press("2", "1")
	h.has("Provider codex", "1 base_url -", "2 proxy -", "3 peak hours…")
	h.lacks("2 api_key_env")

	h = newTUI(t, uiConfig)
	h.press("2", "2")
	h.has("Provider work", "type claude", "profiles work", "nothing here")
	h.lacks("base_url", "peak hours")
}

func TestTUIEditsAProvider(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("2", "3", "1")
	if got := string(h.input().text); got != "https://api.z.ai/api/anthropic" {
		t.Errorf("the field starts with %q", got)
	}
	h.replace("https://example.internal/api")
	h.press("enter")
	h.has("base_url https://example.internal/api", "config.ktav updated")
	h.press("2")
	h.typ("ZAI_KEY")
	h.press("enter")
	pr := h.config().Providers["zai"]
	if pr.BaseURL != "https://example.internal/api" || pr.APIKeyEnv != "ZAI_KEY" {
		t.Errorf("provider: %+v", pr)
	}
	h.press("2", "ctrl+u", "enter")
	if pr := h.config().Providers["zai"]; pr.APIKeyEnv != "" {
		t.Errorf("api_key_env after emptying: %q", pr.APIKeyEnv)
	}

	// An anthropic_compat provider cannot do without its base_url.
	before := h.text()
	h.press("1", "ctrl+u", "enter")
	h.has("error:")
	h.press("esc")
	if h.text() != before {
		t.Error("the refused removal changed the config")
	}
}

func TestTUIAddsAProviderFromTheList(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("2", "a")
	h.has("The kind of backend")
	h.press("3")
	h.typ("https://example.com/anthropic")
	h.press("enter")
	h.replace("raw")
	h.press("enter", "enter")
	h.has("Providers", "raw anthropic_compat raw")
	cfg := h.config()
	if cfg.Providers["raw"] == nil || cfg.Profiles["raw"] == nil {
		t.Error("the provider or its profile is missing")
	}
	if len(h.a.stack) != 2 {
		t.Errorf("%d screens left on the stack, want the list over the main menu", len(h.a.stack))
	}
}

func TestTUIPeakHoursList(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("3")
	h.has("Peak hours: which provider?", "1 codex none", "2 zai none")
	h.lacks("work")
	h.press("2")
	h.has("Peak hours of zai", "zai: no peak hours", "1 add windows", "2 replace all windows", "3 remove a window…",
		"4 time zone", "5 pause", "6 resume (end the pause)", "7 clear all peak hours")
}

func TestTUIEditsPeakHours(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("3", "2", "1")
	h.has("add windows:", "[days] HH:MM-HH:MM")
	h.press("enter")
	h.has("error: enter at least one window")
	h.typ("mon-fri 14:00-18:00")
	h.press("enter")
	h.has("Peak hours of zai", "config.ktav updated, backup kept", "1 mon-fri 14:00-18:00")

	// The zone, as a name or an offset; the state follows.
	h.press("4")
	if got := string(h.input().text); got != "local" {
		t.Errorf("the zone field starts with %q", got)
	}
	h.replace("+08:00")
	h.press("enter")
	h.has("peak hours in +08:00", "now: PEAK, requests are refused until 18:00 +08:00")
	pr := h.config().Providers["zai"]
	if len(pr.Peak) != 1 || pr.Peak[0].Days != "mon-fri" || pr.PeakTZ != "+08:00" {
		t.Errorf("peak hours: %+v in %q", pr.Peak, pr.PeakTZ)
	}

	// Replace all: the field starts with what is there.
	h.press("2")
	if got := string(h.input().text); got != "mon-fri 14:00-18:00" {
		t.Errorf("the field starts with %q", got)
	}
	h.replace("sat,sun 10:00-12:00 20:00-23:00")
	h.press("enter")
	h.has("1 sat,sun 10:00-12:00", "2 20:00-23:00", "now: open")

	// Remove one by picking it.
	h.press("3")
	h.has("Remove which window of zai?", "1 sat,sun 10:00-12:00", "2 20:00-23:00")
	h.press("1")
	h.has("Peak hours of zai", "1 20:00-23:00")
	h.lacks("sat,sun")
	if pr := h.config().Providers["zai"]; len(pr.Peak) != 1 || pr.Peak[0].From != "20:00" {
		t.Errorf("peak hours after the removal: %+v", pr.Peak)
	}

	// Wrong input is answered in the field.
	before := h.text()
	h.press("1")
	h.typ("bogus")
	h.press("enter")
	h.has("error:")
	h.press("esc", "4")
	h.replace("Mars/Base")
	h.press("enter")
	h.has("error:")
	h.press("esc")
	if h.text() != before {
		t.Error("wrong input changed the config")
	}
}

func TestTUIClearsPeakHours(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("3", "2", "1")
	h.typ("mon-fri 14:00-18:00")
	h.press("enter", "3")
	h.has("Remove which window of zai?")
	h.press("esc", "7")
	h.has("Remove all peak hours of zai? [y/N]")
	h.press("n")
	h.has("Peak hours of zai", "1 mon-fri 14:00-18:00")
	h.press("7", "y")
	h.has("config.ktav updated", "zai: no peak hours")
	if h.text() != uiConfig {
		t.Errorf("clear did not give the config back:\n%s", h.text())
	}
	h.press("3")
	h.has("there are no windows to remove")
}

func TestTUIPausesAndResumesPeakHours(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("3", "2", "1")
	h.typ("mon-fri 14:00-18:00")
	h.press("enter", "4")
	h.replace("+08:00")
	h.press("enter", "5")
	h.has("pause:", "empty: until the running peak ends")
	h.press("enter") // until the end of this peak
	h.has("peak hours of zai do not refuse requests until 18:00 +08:00", "now: peak, but paused")
	h.press("6")
	h.has("peak hours of zai apply again", "now: PEAK")
	if _, ok := config.NewPeakPauses(h.store.Dir).Until("zai", peakNow); ok {
		t.Error("the pause survived the resume")
	}
	// A length; and outside the peak a pause needs one.
	h.press("5")
	h.typ("2h")
	h.press("enter")
	if _, ok := config.NewPeakPauses(h.store.Dir).Until("zai", peakNow); !ok {
		t.Error("no pause after 2h")
	}
	h.press("6", "2")
	h.replace("sat,sun 10:00-12:00")
	h.press("enter", "5", "enter")
	h.has("error:", "not peak time")
}

func TestTUIPeakHoursOfAProvider(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("2", "3", "4")
	h.has("Peak hours of zai")
	h.press("1")
	h.typ("mon-fri 14:00-18:00")
	h.press("enter", "4")
	h.replace("+08:00")
	h.press("enter", "esc")
	h.has("Provider zai", "peak hours mon-fri 14:00-18:00 (+08:00 time); PEAK NOW")
	h.press("esc")
	h.has("Providers", "zai anthropic_compat zai [peak hours]")
	h.press("esc", "3")
	h.has("2 zai mon-fri 14:00-18:00 (+08:00 time); PEAK NOW")
}

func TestTUIScrollsLongLists(t *testing.T) {
	if s, e := window(10, 0, 5); s != 0 || e != 5 {
		t.Errorf("window(10, 0, 5) = %d, %d", s, e)
	}
	if s, e := window(10, 9, 5); s != 5 || e != 10 {
		t.Errorf("window(10, 9, 5) = %d, %d", s, e)
	}
	if s, e := window(10, 5, 5); s != 3 || e != 8 {
		t.Errorf("window(10, 5, 5) = %d, %d", s, e)
	}
	if s, e := window(3, 1, 5); s != 0 || e != 3 {
		t.Errorf("window(3, 1, 5) = %d, %d", s, e)
	}

	h := newTUI(t, uiConfig)
	h.a.Update(tea.WindowSizeMsg{Width: 80, Height: 16})
	var items []menuItem
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "b10", "b11", "b12"} {
		items = append(items, menuItem{label: n, run: func(*app) tea.Cmd { return nil }})
	}
	h.a.push(&menu{head: func(*app) string { return "Long" }, items: func(*app) []menuItem { return items }})
	h.has("Long", "a1", "↓ more")
	h.lacks("↑ more", "b12")
	for range 11 {
		h.press("down")
	}
	h.has("b12", "↑ more")
	h.lacks("a1 ", "↓ more")
	h.press("down") // wraps to the top
	h.has("a1", "↓ more")
}

func TestTUIMenuKeys(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("j", "j", "k")
	h.press("enter")
	h.has("Providers")
	h.press("h")
	h.has("1 Profiles")
	h.press("l") // the cursor stays on Providers
	h.has("Providers")
	h.press("backspace")
	h.press("9") // no ninth item: nothing happens
	h.has("1 Profiles")
	h.a.errMsg = "something"
	h.has("error: something")
}

func TestTUIShowsATextForTheEmptyAndGoneThings(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.a.push(profileMenu("nope"))
	h.has("Profile nope", "this profile is gone", "nothing here")
	h.press("enter", "esc")
	h.a.push(providerMenu("nope"))
	h.has("this provider is gone")
	h.a.push(peakMenu("nope"))
	h.lacks("add windows")
}

func TestTUINeedsATerminalAndAConfigFile(t *testing.T) {
	if err := interactiveCmd([]string{"extra"}); err == nil {
		t.Error("an argument was accepted")
	}
	if err := interactiveCmd(nil); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Errorf("without a terminal: %v", err)
	}
	t.Setenv("CCGP_HOME", t.TempDir())
	t.Setenv(config.ConfigFileEnv, "")
	t.Setenv("PROXY_TOKEN", strings.Repeat("t", 64))
	t.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")
	if _, err := newApp(creds.Store{Dir: t.TempDir()}, t.TempDir(), time.Now); err == nil || !strings.Contains(err.Error(), "needs config.ktav") {
		t.Errorf("env-only mode: %v", err)
	}
}

func TestInteractiveCommandIsRegistered(t *testing.T) {
	for _, name := range []string{"--interactive", "-i"} {
		inv, err := parseInvocation([]string{name})
		if err != nil || inv.cmd == nil || inv.cmd.name != name || !inv.cmd.ensureConfig {
			t.Errorf("%s: %+v, %v", name, inv, err)
		}
	}
	// Nothing but the flag starts it: a bare word is a profile, no word is help.
	if inv, _ := parseInvocation([]string{"interactive"}); inv.cmd != nil || inv.profile != "interactive" {
		t.Errorf("a bare word started a command: %+v", inv)
	}
	if inv, _ := parseInvocation(nil); inv.cmd != nil {
		t.Errorf("no arguments started a command: %+v", inv)
	}
}
