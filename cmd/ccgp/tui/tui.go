package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/core"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/sess"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgedit"
	peakcfg "github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

var (
	styTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	styCursor = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	styDim    = lipgloss.NewStyle().Faint(true)
	styOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styCaret  = lipgloss.NewStyle().Reverse(true)
)

// screen is one thing on the stack.
type screen interface {
	header(a *app) string
	body(a *app) string
	key(a *app, k tea.KeyMsg) tea.Cmd
	help() string
}

// app is the tea.Model.
type app struct {
	cfg        *config.Config
	store      creds.Store
	claudeBase string
	pauses     *peakcfg.Pauses
	now        func() time.Time

	// run starts `ccgp args…` for the screens that do what a command does:
	// with the terminal (login, update) or with its output caught and shown.
	run func(title string, args []string, terminal bool) tea.Cmd

	stack       []screen
	msg, errMsg string
	width       int
	height      int
	quit        bool
}

func newApp(store creds.Store, claudeBase string, now func() time.Time) (*app, error) {
	a := &app{store: store, claudeBase: claudeBase, now: now, pauses: peakcfg.NewPauses(store.Dir), run: runSelf}
	if err := a.reload(); err != nil {
		return nil, err
	}
	a.stack = []screen{mainMenu()}
	return a, nil
}

// reload reads the config afresh; the old one stays when it cannot be read.
func (a *app) reload() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.IsEnvMode() {
		return errors.New("ccgp --interactive needs config.ktav (env-only mode has a single implicit profile)")
	}
	a.cfg = cfg
	return nil
}

func (a *app) top() screen { return a.stack[len(a.stack)-1] }

func (a *app) push(s screen) { a.stack = append(a.stack, s) }

// pop removes the top screen; the root stays.
func (a *app) pop() {
	if len(a.stack) > 1 {
		a.stack = a.stack[:len(a.stack)-1]
	}
}

// popTo leaves n screens on the stack.
func (a *app) popTo(n int) {
	if n < 1 {
		n = 1
	}
	if n < len(a.stack) {
		a.stack = a.stack[:n]
	}
}

// back goes one screen up, and quits from the root.
func (a *app) back() tea.Cmd {
	if len(a.stack) > 1 {
		a.pop()
		return nil
	}
	a.quit = true
	return tea.Quit
}

// edit applies an edit of config.ktav (validated, backed up), reads the config
// again and says what happened.
func (a *app) edit(fn func(text string) (string, error)) error {
	backup, err := cfgedit.EditFile(a.cfg.ConfigFile, fn)
	if err != nil {
		return err
	}
	if backup == "" {
		a.msg = "config.ktav already had this"
	} else {
		a.msg = "config.ktav updated, backup " + filepath.Base(backup)
	}
	return a.reload()
}

func (a *app) Init() tea.Cmd { return nil }

func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = m.Width, m.Height
	case outputMsg:
		a.msg, a.errMsg = "", ""
		switch {
		case m.text != "":
			a.push(&viewScreen{title: m.title, text: m.text, tail: m.tail})
			if m.err != nil {
				a.errMsg = m.err.Error()
			}
		case m.err != nil:
			a.errMsg = m.err.Error()
		default:
			a.msg = m.title + ": done"
		}
		_ = a.reload() // a command may have changed the config
	case termDoneMsg:
		a.msg, a.errMsg = "", ""
		if m.err != nil {
			a.errMsg = m.title + ": " + m.err.Error()
		} else {
			a.msg = m.title + ": done"
		}
		_ = a.reload()
	case tea.KeyMsg:
		if m.String() == "ctrl+c" {
			a.quit = true
			return a, tea.Quit
		}
		a.msg, a.errMsg = "", ""
		cmd := a.top().key(a, m)
		if a.quit {
			return a, tea.Quit
		}
		return a, cmd
	}
	return a, nil
}

func (a *app) View() string {
	s := a.top()
	var b strings.Builder
	b.WriteString(styTitle.Render(s.header(a)) + "\n\n")
	if body := s.body(a); body != "" {
		b.WriteString(body + "\n")
	}
	if a.msg != "" {
		b.WriteString("\n" + styOK.Render(a.msg) + "\n")
	}
	if a.errMsg != "" {
		b.WriteString("\n" + styErr.Render("error: "+a.errMsg) + "\n")
	}
	b.WriteString("\n" + styDim.Render(s.help()))
	return b.String()
}

// listRows is how many menu rows fit.
func (a *app) listRows() int {
	if a.height <= 0 {
		return 18
	}
	if r := a.height - 12; r > 5 {
		return r
	}
	return 5
}

// ---- menu ----

type menuItem struct {
	// id names the item for the parity test (parity_test.go), which crawls every
	// screen and ties each id path to a command line. An item made from data (a
	// profile, a provider, a window) has the id of its kind.
	id    string
	label string
	hot   string // a key that picks the item, besides its number
	run   func(a *app) tea.Cmd
}

// menu is a list to pick from. head, info and items are asked each time it is
// drawn, from the current config.
type menu struct {
	head   func(a *app) string
	info   func(a *app) string
	items  func(a *app) []menuItem
	keys   string // the help line, when not the usual one
	cursor int
}

func (m *menu) header(a *app) string { return m.head(a) }

func (m *menu) help() string {
	if m.keys != "" {
		return m.keys
	}
	return "↑/↓ move · enter or number select · esc back · ctrl+c quit"
}

func (m *menu) clamp(n int) {
	if m.cursor >= n {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *menu) key(a *app, k tea.KeyMsg) tea.Cmd {
	items := m.items(a)
	m.clamp(len(items))
	s := k.String()
	switch s {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		} else if len(items) > 0 {
			m.cursor = len(items) - 1
		}
	case "down", "j":
		if m.cursor < len(items)-1 {
			m.cursor++
		} else {
			m.cursor = 0
		}
	case "enter", "right", "l":
		if len(items) > 0 {
			return items[m.cursor].run(a)
		}
	case "esc", "left", "h", "backspace", "q":
		return a.back()
	default:
		for i, it := range items {
			if it.hot != "" && it.hot == s {
				m.cursor = i
				return it.run(a)
			}
		}
		if len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
			if i := int(s[0] - '1'); i < len(items) {
				m.cursor = i
				return items[i].run(a)
			}
		}
	}
	return nil
}

func (m *menu) body(a *app) string {
	var b strings.Builder
	if m.info != nil {
		if s := m.info(a); s != "" {
			b.WriteString(s + "\n\n")
		}
	}
	items := m.items(a)
	m.clamp(len(items))
	if len(items) == 0 {
		b.WriteString(styDim.Render("nothing here"))
		return b.String()
	}
	start, end := window(len(items), m.cursor, a.listRows())
	if start > 0 {
		b.WriteString(styDim.Render("  ↑ more") + "\n")
	}
	for i := start; i < end; i++ {
		num := "  "
		if i < 9 {
			num = fmt.Sprintf("%d ", i+1)
		}
		line := num + items[i].label
		if i == m.cursor {
			line = styCursor.Render("› " + line)
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
	}
	if end < len(items) {
		b.WriteString(styDim.Render("  ↓ more") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// window picks the rows [start, end) of n that show the cursor, rows at most.
func window(n, cursor, rows int) (start, end int) {
	if n <= rows {
		return 0, n
	}
	start = cursor - rows/2
	if start < 0 {
		start = 0
	}
	if start+rows > n {
		start = n - rows
	}
	return start, start + rows
}

// ---- one-line input ----

// input asks for a line. submit returns the screen to show next (nil: none)
// or an error, which the input shows and stays open for.
type input struct {
	head, label, hint string
	text              []rune
	pos               int
	err               string
	submit            func(a *app, value string) (screen, error)
	pending           tea.Cmd // set by submit: what to run once the input is gone
}

func newInput(head, label, hint, initial string, submit func(a *app, value string) (screen, error)) *input {
	t := []rune(initial)
	return &input{head: head, label: label, hint: hint, text: t, pos: len(t), submit: submit}
}

func (in *input) header(a *app) string { return in.head }

func (in *input) help() string {
	return "enter accept · esc cancel · ctrl+u clears · ctrl+c quit"
}

func (in *input) body(a *app) string {
	var b strings.Builder
	if in.hint != "" {
		b.WriteString(styDim.Render(in.hint) + "\n\n")
	}
	before, at, after := string(in.text[:in.pos]), " ", ""
	if in.pos < len(in.text) {
		at, after = string(in.text[in.pos]), string(in.text[in.pos+1:])
	}
	b.WriteString(in.label + ": " + before + styCaret.Render(at) + after)
	if in.err != "" {
		b.WriteString("\n\n" + styErr.Render("error: "+in.err))
	}
	return b.String()
}

func (in *input) insert(r []rune) {
	in.text = append(in.text[:in.pos], append(append([]rune{}, r...), in.text[in.pos:]...)...)
	in.pos += len(r)
}

func (in *input) key(a *app, k tea.KeyMsg) tea.Cmd {
	switch k.Type {
	case tea.KeyEnter:
		next, err := in.submit(a, strings.TrimSpace(string(in.text)))
		if err != nil {
			in.err = err.Error()
			return nil
		}
		if a.top() == screen(in) {
			a.pop()
		}
		if next != nil {
			a.push(next)
		}
		cmd := in.pending
		in.pending = nil
		return cmd
	case tea.KeyEsc:
		a.pop()
	case tea.KeyLeft:
		if in.pos > 0 {
			in.pos--
		}
	case tea.KeyRight:
		if in.pos < len(in.text) {
			in.pos++
		}
	case tea.KeyHome, tea.KeyCtrlA:
		in.pos = 0
	case tea.KeyEnd, tea.KeyCtrlE:
		in.pos = len(in.text)
	case tea.KeyBackspace:
		if in.pos > 0 {
			in.text = append(in.text[:in.pos-1], in.text[in.pos:]...)
			in.pos--
		}
	case tea.KeyDelete:
		if in.pos < len(in.text) {
			in.text = append(in.text[:in.pos], in.text[in.pos+1:]...)
		}
	case tea.KeyCtrlU:
		in.text, in.pos = in.text[in.pos:], 0
	case tea.KeyCtrlK:
		in.text = in.text[:in.pos]
	case tea.KeyCtrlW:
		end := in.pos
		for in.pos > 0 && in.text[in.pos-1] == ' ' {
			in.pos--
		}
		for in.pos > 0 && in.text[in.pos-1] != ' ' {
			in.pos--
		}
		in.text = append(in.text[:in.pos], in.text[end:]...)
	case tea.KeySpace:
		in.insert([]rune{' '})
	case tea.KeyRunes:
		in.insert(k.Runes)
	}
	return nil
}

// ---- confirmation ----

type confirmScreen struct {
	head, question string
	yes            func(a *app) error
	then           func(a *app) tea.Cmd // optional: a command to run after a yes that worked
}

func (c *confirmScreen) header(a *app) string { return c.head }

func (c *confirmScreen) help() string { return "y yes · n or esc no · ctrl+c quit" }

func (c *confirmScreen) body(a *app) string { return c.question + " [y/N]" }

func (c *confirmScreen) key(a *app, k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "y", "Y":
		if a.top() == screen(c) {
			a.pop()
		}
		if c.yes != nil {
			if err := c.yes(a); err != nil {
				a.errMsg = err.Error()
				return nil
			}
		}
		if c.then != nil {
			return c.then(a)
		}
	case "n", "N", "esc", "enter":
		a.pop()
	}
	return nil
}

// outputMsg is the end of a command run with its output caught.
type outputMsg struct {
	title string
	text  string
	err   error
	tail  bool // show the end of the text first (a log)
}

// termDoneMsg is the end of a command that had the terminal.
type termDoneMsg struct {
	title string
	err   error
}

// runSelf is app.run in the program: it starts this very executable.
func runSelf(title string, args []string, terminal bool) tea.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return outputMsg{title: title, err: err} }
	}
	c := exec.Command(exe, args...)
	if terminal {
		return tea.ExecProcess(c, func(err error) tea.Msg { return termDoneMsg{title: title, err: err} })
	}
	tail := len(args) > 0 && args[0] == "--logs"
	return func() tea.Msg {
		out, err := c.CombinedOutput()
		return outputMsg{title: title, text: strings.TrimRight(string(out), "\r\n"), err: err, tail: tail}
	}
}

// viewScreen shows the output of a command, scrollable.
type viewScreen struct {
	title, text string
	tail        bool
	top         int
	placed      bool
}

func (v *viewScreen) header(a *app) string { return v.title }

func (v *viewScreen) help() string {
	return "↑/↓ pgup/pgdn home/end scroll · esc back · ctrl+c quit"
}

func (v *viewScreen) rows(a *app) int {
	if a.height <= 0 {
		return 18
	}
	if r := a.height - 7; r > 5 {
		return r
	}
	return 5
}

func (v *viewScreen) clamp(lines, rows int) {
	if !v.placed {
		v.placed = true
		if v.tail {
			v.top = lines - rows
		}
	}
	if v.top > lines-rows {
		v.top = lines - rows
	}
	if v.top < 0 {
		v.top = 0
	}
}

func (v *viewScreen) body(a *app) string {
	lines := strings.Split(v.text, "\n")
	rows := v.rows(a)
	v.clamp(len(lines), rows)
	end := v.top + rows
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	if v.top > 0 {
		b.WriteString(styDim.Render("↑ more") + "\n")
	}
	b.WriteString(strings.Join(lines[v.top:end], "\n"))
	if end < len(lines) {
		b.WriteString("\n" + styDim.Render("↓ more"))
	}
	return b.String()
}

func (v *viewScreen) key(a *app, k tea.KeyMsg) tea.Cmd {
	lines := len(strings.Split(v.text, "\n"))
	rows := v.rows(a)
	v.clamp(lines, rows)
	switch k.String() {
	case "up", "k":
		v.top--
	case "down", "j":
		v.top++
	case "pgup":
		v.top -= rows
	case "pgdown", " ":
		v.top += rows
	case "home", "g":
		v.top = 0
	case "end", "G":
		v.top = lines
	case "esc", "q", "left", "h", "backspace", "enter":
		a.pop()
		return nil
	}
	v.clamp(lines, rows)
	return nil
}

const interactiveUsage = "usage: ccgp --interactive (or -i)"

// interactiveCmd runs `ccgp --interactive`: a full-screen Bubble Tea interface
// to look at, change and add profiles and providers (tui.go, tui_screens.go).
// Nothing starts it but the flag.
func InteractiveCmd(args []string) error {
	if len(args) != 0 {
		return errors.New(interactiveUsage)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("ccgp --interactive needs a terminal: it draws menus and reads keys")
	}
	store, err := common.CredsStore()
	if err != nil {
		return err
	}
	base, err := common.ClaudeHomeBase()
	if err != nil {
		return err
	}
	a, err := newApp(store, base, time.Now)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(a, tea.WithAltScreen()).Run()
	return err
}

// errNotPicked ends a list closed without a choice.
var errNotPicked = errors.New("no profile chosen")

// launchPicked asks on the terminal which profile to run, then runs it with
// claudeArgs. The choice claude is your own Claude Code, run as plain claude.
func LaunchPicked(claudeArgs []string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("ccgp . needs a terminal to choose a profile; name one instead: ccgp <profile>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := common.CredsStore()
	if err != nil {
		return err
	}
	base, err := common.ClaudeHomeBase()
	if err != nil {
		return err
	}
	picked := ""
	a := &app{cfg: cfg, store: store, claudeBase: base, now: time.Now, run: runSelf, stack: []screen{pickMenu(&picked)}}
	if _, err := tea.NewProgram(a, tea.WithAltScreen()).Run(); err != nil {
		return err
	}
	return runPicked(picked, claudeArgs)
}

// runPicked runs what the list returned: nothing, plain claude or a profile.
func runPicked(picked string, claudeArgs []string) error {
	switch picked {
	case "":
		return errNotPicked
	case sess.ClaudeEndpointLabel:
		return launchPlainClaude(claudeArgs)
	}
	return core.Launch(picked, claudeArgs)
}

// pickMenu lists the profiles and your own Claude Code; choosing one sets
// *picked and quits.
func pickMenu(picked *string) *menu {
	choose := func(name string) func(a *app) tea.Cmd {
		return func(a *app) tea.Cmd {
			*picked = name
			a.quit = true
			return tea.Quit
		}
	}
	return &menu{
		head: func(*app) string { return "Run Claude Code with which profile?" },
		keys: "↑/↓ move · enter or number run · c your own Claude Code · esc, q or ctrl+c leave without running",
		items: func(a *app) []menuItem {
			var items []menuItem
			for _, r := range core.BuildProfileRows(a.cfg, a.store, a.claudeBase) {
				items = append(items, menuItem{
					id: "profile", label: fmt.Sprintf("%-14s %-14s %-17s %s", r.Name, r.Provider, r.Type, r.Login),
					run: choose(r.Name),
				})
			}
			return append(items, menuItem{
				id: sess.ClaudeEndpointLabel, label: fmt.Sprintf("%-14s %s", sess.ClaudeEndpointLabel, "your own Claude Code, as plain claude (no ccgp)"),
				hot: "c", run: choose(sess.ClaudeEndpointLabel),
			})
		},
	}
}

// launchPlainClaude runs claude with the arguments and the environment as they
// are: what typing claude here does.
func launchPlainClaude(claudeArgs []string) error {
	claude, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("claude not found in PATH")
	}
	return core.RunClaude(claude, claudeArgs, os.Environ())
}
