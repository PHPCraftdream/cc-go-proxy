package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

// The Bubble Tea model behind `ccgp --interactive`: a stack of screens (menus,
// one-line inputs, confirmations) over a config that is read afresh after every
// change. Screens build what they show from app.cfg, so a list is never stale.

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
	pauses     *config.PeakPauses
	now        func() time.Time

	stack       []screen
	msg, errMsg string
	width       int
	height      int
	quit        bool
}

func newApp(store creds.Store, claudeBase string, now func() time.Time) (*app, error) {
	a := &app{store: store, claudeBase: claudeBase, now: now, pauses: config.NewPeakPauses(store.Dir)}
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
	backup, err := config.EditFile(a.cfg.ConfigFile, fn)
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
	cursor int
}

func (m *menu) header(a *app) string { return m.head(a) }

func (m *menu) help() string {
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
}

func (c *confirmScreen) header(a *app) string { return c.head }
func (c *confirmScreen) help() string         { return "y yes · n or esc no · ctrl+c quit" }
func (c *confirmScreen) body(a *app) string   { return c.question + " [y/N]" }

func (c *confirmScreen) key(a *app, k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "y", "Y":
		if a.top() == screen(c) {
			a.pop()
		}
		if err := c.yes(a); err != nil {
			a.errMsg = err.Error()
		}
	case "n", "N", "esc", "enter":
		a.pop()
	}
	return nil
}
