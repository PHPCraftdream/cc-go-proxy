package main

import (
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Screens that do what a command does run that command: `ccgp args…` is started
// as a process of its own, so what happens here is what happens on the command
// line, down to the words it prints. A command that talks to the user (a
// sign-in, an update) gets the terminal while it runs; the others are run with
// their output caught and shown on a screen of its own.

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
