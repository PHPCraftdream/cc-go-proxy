package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
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
	args, fork, force, err := splitSessionFlags(args)
	if err != nil {
		return err
	}
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
		Within:        cwd,
		Fork:          fork,
		Force:         force,
	}
	return copySessions(os.Stdout, label, src, dst, count, opt)
}

// sessionVerbs are the flags of --send, --import and --export.
var sessionVerbs = []string{"--fork", "--force"}

// splitSessionFlags takes --fork and --force out of the arguments of the
// session commands.
func splitSessionFlags(args []string) (rest []string, fork, force bool, err error) {
	for _, a := range args {
		switch a {
		case "--fork":
			fork = true
		case "--force":
			force = true
		default:
			rest = append(rest, a)
		}
	}
	if fork && force {
		return nil, false, false, errors.New("--fork and --force exclude each other: --fork keeps both versions, --force replaces the destination")
	}
	return rest, fork, force, nil
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
		return "", "", 0, fmt.Errorf("usage: ccgp --send <from> <to> [count] [--fork|--force]   (from/to: \"claude\" or a profile; count: how many latest sessions of the current directory, default %d)", defaultSessionCount)
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
		return "", 0, fmt.Errorf("usage: ccgp --%s <profile> [count] [--fork|--force]   (count: how many latest sessions of the current directory, default %d)", kind, defaultSessionCount)
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
	// A transcript of gigabytes goes through in lines that are mostly garbage at
	// once: the collector runs a quarter as often (the heap stays small).
	defer debug.SetGCPercent(debug.SetGCPercent(400))
	var done, same, skipped, diverged, elsewhere, failed, stripped int
	var forks []string
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
		if s.Into != "" {
			line += "  (of a subdirectory, written into " + s.Into + ")"
		}
		if note := artifactsNote(r); note != "" {
			line += "  (" + note + ")"
		}
		if note := failedNote(r); note != "" {
			line += "  (" + note + ")"
		}
		switch {
		case r.Err != nil:
			failed++
			line = fmt.Sprintf("  %-9s %s: %v", "failed", id, r.Err)
		case r.Status == sessions.Copied || r.Status == sessions.Updated:
			done++
			stripped += r.Removed
		case r.Status == sessions.Forked:
			done++
			stripped += r.Removed
			line += "  (both versions kept: this one is the new session " + r.Fork + ")"
			forks = append(forks, r.Fork)
		case r.Status == sessions.Replaced:
			done++
			stripped += r.Removed
			line += "  (the old one is kept as " + r.Backup + ")"
		case r.Status == sessions.Unchanged:
			same++
			if r.Fork != "" {
				line += "  (as the session " + r.Fork + ")"
				forks = append(forks, r.Fork)
			}
		default:
			skipped++
			switch r.Status {
			case sessions.Diverged:
				diverged++
			case sessions.Elsewhere:
				elsewhere++
			}
			line += "  (" + skipReason(r) + ")"
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
	if dst.provider != "" && len(forks) > 0 {
		_, _ = fmt.Fprintf(w, "open a fork with: ccgp %s --resume %s\n", dst.label, forks[0])
	}
	if dst.provider == "" && len(forks) > 0 {
		_, _ = fmt.Fprintf(w, "open it with: claude --resume %s (in the directory it was written for)\n", forks[0])
	}
	if diverged > 0 && !opt.Fork && !opt.Force {
		_, _ = fmt.Fprintln(w, "diverged sessions: --fork writes them as new sessions next to the old ones, --force replaces the old ones (kept as .bak-<time>)")
	}
	if elsewhere > 0 && !opt.Fork {
		_, _ = fmt.Fprintln(w, "a session id that is another session there: --fork writes it under a new id")
	}
	if failed > 0 {
		return exitError(1)
	}
	return nil
}

// artifactsNote tells what came with the transcript from outside it.
func artifactsNote(r sessions.Result) string {
	var parts []string
	if r.Tasks > 0 {
		parts = append(parts, fmt.Sprintf("task list: %d files", r.Tasks))
	}
	if r.History > 0 {
		parts = append(parts, fmt.Sprintf("file versions for /rewind: %d", r.History))
	}
	if r.Plans > 0 {
		parts = append(parts, fmt.Sprintf("plan: %d", r.Plans))
	}
	return strings.Join(parts, ", ")
}

// failedNote tells that a written transcript was left without its failed end.
func failedNote(r sessions.Result) string {
	switch r.Status {
	case sessions.Copied, sessions.Updated, sessions.Forked, sessions.Replaced:
		if r.Failed > 0 {
			return fmt.Sprintf("left out its failed end: %d API errors with no reply after them", r.Failed)
		}
	}
	return ""
}

func skipReason(r sessions.Result) string {
	switch {
	case r.Status == sessions.Ahead:
		return "the destination already has more of it"
	case r.Status == sessions.Elsewhere && r.Fork != "":
		return "its fork " + r.Fork + " is in " + r.Other + " already, for another directory; not written a second time"
	case r.Status == sessions.Elsewhere:
		return "the destination has a session of this id in " + r.Other + ", for another directory; not written next to it"
	case r.Fork != "":
		return "both sides changed; the fork " + r.Fork + " went its own way too and was left alone"
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

// seedProfileSessions runs seedFirstSession for a profile about to start; any
// trouble only prints a note, starting the profile matters more.
func seedProfileSessions(w io.Writer, cfg *config.Config, profile string) {
	note := func(err error) { _, _ = fmt.Fprintf(w, "first launch: sessions not copied: %v\n", err) }
	base, err := claudeHomeBase()
	if err != nil {
		note(err)
		return
	}
	official, _, err := officialClaudeDir(base)
	if err != nil {
		note(err)
		return
	}
	src, err := resolveEndpoint(cfg, base, official, claudeEndpointLabel)
	if err != nil {
		note(err)
		return
	}
	dst, err := resolveEndpoint(cfg, base, official, profile)
	if err != nil {
		note(err)
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		note(err)
		return
	}
	seedFirstSession(w, src, dst, cwd)
}

// seedFirstSession gives a profile that has no session of the working
// directory the latest one of that directory from the user's own Claude Code,
// so the launch can go on with the work (/resume, -c: Claude Code lists the
// sessions of the current directory only, whatever the profile has elsewhere).
// A profile that already has a session of this directory, or a directory
// without one in Claude, is left alone.
func seedFirstSession(w io.Writer, src, dst endpoint, cwd string) {
	project := projectDirName(cwd)
	has := func(dir string) (bool, error) {
		list, err := sessions.List(dir)
		for _, s := range list {
			if strings.EqualFold(s.Project, project) {
				return true, err
			}
		}
		return false, err
	}
	if have, err := has(dst.dir); err != nil || have {
		return
	}
	if found, err := has(src.dir); err != nil || !found {
		return
	}
	opt := sessions.Options{StripThinking: stripThinkingBetween(src, dst), Project: project}
	if err := copySessions(w, "first launch of "+dst.label, src, dst, 1, opt); err != nil {
		_, _ = fmt.Fprintf(w, "first launch: sessions not copied: %v\n", err)
	}
}
