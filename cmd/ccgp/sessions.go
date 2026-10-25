package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/sessions"
)

const defaultSessionCount = 10

// endpoint is one side of a send: a profile's isolated Claude dir or the
// user's own one. An empty provider marks the user's own Claude Code dir,
// which is a provider of its own for the thinking rule.
type endpoint struct {
	dir      string
	provider string
	label    string // profile name or "claude"
}

// claudeEndpointLabel names the user's own Claude Code dir in `ccgp --send`.
const claudeEndpointLabel = "claude"

// sessionsCmd implements `ccgp --send <from> <to> [count]` and its aliases
// `ccgp --import|--export <profile> [count]`, all through the same code path.
// Only the sessions of the directory the command is run in are copied.
func sessionsCmd(kind string, args []string) error {
	from, to, count, err := parseSendCommand(kind, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	base, err := claudeHomeBase()
	if err != nil {
		return err
	}
	official, _, err := officialClaudeDir(base)
	if err != nil {
		return err
	}
	src, err := resolveEndpoint(cfg, base, official, from)
	if err != nil {
		return err
	}
	dst, err := resolveEndpoint(cfg, base, official, to)
	if err != nil {
		return err
	}
	if src.dir == dst.dir {
		return fmt.Errorf("send: from and to are the same (%s)", src.label)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	label := "send " + from + " -> " + to
	if kind != "send" {
		// Aliases keep the command the user typed in the report.
		label = kind + " " + to
		if kind == "export" {
			label = kind + " " + from
		}
	}
	opt := sessions.Options{
		StripThinking: stripThinkingBetween(src, dst),
		Project:       projectDirName(cwd),
	}
	return copySessions(os.Stdout, label, src, dst, count, opt)
}

// projectDirName is how Claude Code names the projects/ folder of a working
// directory: every character outside [A-Za-z0-9] becomes "-" (two for a
// character outside the BMP, as it counts UTF-16 units).
func projectDirName(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// parseSendCommand adapts the aliases to the send arguments.
func parseSendCommand(kind string, args []string) (from, to string, count int, err error) {
	switch kind {
	case "import":
		profile, n, e := parseSessionArgs("import", args)
		return "claude", profile, n, e
	case "export":
		profile, n, e := parseSessionArgs("export", args)
		return profile, "claude", n, e
	}
	return parseSendArgs(args)
}

func parseSendArgs(args []string) (from, to string, count int, err error) {
	if len(args) < 2 || len(args) > 3 {
		return "", "", 0, fmt.Errorf("usage: ccgp --send <from> <to> [count]   (from/to: \"claude\" or a profile; count: how many latest sessions of the current directory, default %d)", defaultSessionCount)
	}
	count = defaultSessionCount
	if len(args) == 3 {
		n, convErr := strconv.Atoi(args[2])
		if convErr != nil || n < 1 {
			return "", "", 0, fmt.Errorf("count must be a positive number, got %q", args[2])
		}
		count = n
	}
	return args[0], args[1], count, nil
}

func parseSessionArgs(kind string, args []string) (profile string, count int, err error) {
	if len(args) < 1 || len(args) > 2 {
		return "", 0, fmt.Errorf("usage: ccgp --%s <profile> [count]   (count: how many latest sessions of the current directory, default %d)", kind, defaultSessionCount)
	}
	count = defaultSessionCount
	if len(args) == 2 {
		n, convErr := strconv.Atoi(args[1])
		if convErr != nil || n < 1 {
			return "", 0, fmt.Errorf("count must be a positive number, got %q", args[1])
		}
		count = n
	}
	return args[0], count, nil
}

// resolveEndpoint maps "claude" or a configured profile name to its dir.
func resolveEndpoint(cfg *config.Config, claudeBase, officialDir, name string) (endpoint, error) {
	if name == claudeEndpointLabel {
		return endpoint{dir: officialDir, label: claudeEndpointLabel}, nil
	}
	pf, ok := cfg.Profiles[name]
	if !ok {
		return endpoint{}, fmt.Errorf("unknown profile %q (send between: %s, %s)", name, names(cfg.Profiles), claudeEndpointLabel)
	}
	dir, err := claudeProfileDir(claudeBase, name)
	if err != nil {
		return endpoint{}, err
	}
	return endpoint{dir: dir, provider: pf.Provider, label: name}, nil
}

// stripThinkingBetween keeps thinking blocks only when both endpoints are
// profiles on the same provider: their signatures are only valid there.
func stripThinkingBetween(from, to endpoint) bool {
	return from.provider == "" || to.provider == "" || from.provider != to.provider
}

// copySessions copies and reports as it goes (transcripts can be gigabytes);
// it is the testable core of send. label opens the first output line.
func copySessions(w io.Writer, label string, src, dst endpoint, count int, opt sessions.Options) error {
	var done, same, skipped, failed, stripped int
	opened := false
	open := func() {
		if !opened {
			opened = true
			_, _ = fmt.Fprintf(w, "%s: %s -> %s\n", label, src.dir, dst.dir)
			if opt.Project != "" {
				_, _ = fmt.Fprintf(w, "  directory: %s\n", opt.Project)
			}
		}
	}
	opt.OnStart = func(i, n int, s sessions.Session) {
		open()
		_, _ = fmt.Fprintf(w, "  [%d/%d] %s  %s ...\n", i, n, s.Project+"/"+shortID(s.ID), humanSize(s.Size))
	}
	opt.OnProgress = func(_ sessions.Session, got, total int64) {
		if total > 0 {
			_, _ = fmt.Fprintf(w, "      %3d%%  %s of %s\n", got*100/total, humanSize(got), humanSize(total))
		}
	}
	opt.OnResult = func(r sessions.Result) {
		s := r.Session
		id := s.Project + "/" + shortID(s.ID)
		line := fmt.Sprintf("  %-9s %s  %s  %s", r.Status, s.ModTime.Format("2006-01-02 15:04"), id, humanSize(s.Size))
		switch {
		case r.Err != nil:
			failed++
			line = fmt.Sprintf("  %-9s %s: %v", "failed", id, r.Err)
		case r.Status == sessions.Copied || r.Status == sessions.Updated:
			done++
			stripped += r.Removed
		case r.Status == sessions.Unchanged:
			same++
		default:
			skipped++
			line += "  (" + skipReason(r.Status) + ")"
		}
		_, _ = fmt.Fprintln(w, line)
	}

	results, err := sessions.Copy(src.dir, dst.dir, count, opt)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		where := filepath.Join(src.dir, "projects")
		if opt.Project != "" {
			where = filepath.Join(where, opt.Project)
		}
		_, _ = fmt.Fprintf(w, "no sessions found in %s\n", where)
		return nil
	}

	_, _ = fmt.Fprintf(w, "%d written, %d unchanged, %d skipped, %d failed\n", done, same, skipped, failed)
	if opt.StripThinking && stripped > 0 {
		_, _ = fmt.Fprintf(w, "removed %d thinking blocks (their signatures are only valid for the provider that wrote them)\n", stripped)
	}
	if dst.provider != "" && done > 0 {
		_, _ = fmt.Fprintf(w, "next: ccgp %s -c (or /resume): sessions are listed per project directory\n", dst.label)
	}
	if failed > 0 {
		return exitError(1)
	}
	return nil
}

func skipReason(s sessions.Status) string {
	if s == sessions.Ahead {
		return "the destination already has more of it"
	}
	return "both sides changed; the destination was not overwritten"
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
