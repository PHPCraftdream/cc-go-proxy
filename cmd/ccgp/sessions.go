package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/sessions"
)

const defaultSessionCount = 10

// sessionsCmd implements `ccgp import|export <profile> [count]`.
func sessionsCmd(kind string, args []string) error {
	profile, count, err := parseSessionArgs(kind, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, ok := cfg.Profiles[profile]; !ok {
		return fmt.Errorf("unknown profile %q (configured: %s)", profile, names(cfg.Profiles))
	}
	base, err := claudeHomeBase()
	if err != nil {
		return err
	}
	official, _, err := officialClaudeDir(base)
	if err != nil {
		return err
	}
	isolated := filepath.Join(base, profile)

	src, dst, opt := official, isolated, sessions.Options{}
	if kind == "export" {
		src, dst, opt = isolated, official, sessions.Options{StripThinking: true}
	}
	return copySessions(os.Stdout, kind, profile, src, dst, count, opt)
}

func parseSessionArgs(kind string, args []string) (profile string, count int, err error) {
	if len(args) < 1 || len(args) > 2 {
		return "", 0, fmt.Errorf("usage: ccgp %s <profile> [count]   (count: how many latest sessions, default %d)", kind, defaultSessionCount)
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

// copySessions copies and reports; it is the testable core of import/export.
func copySessions(w io.Writer, kind, profile, src, dst string, count int, opt sessions.Options) error {
	results, err := sessions.Copy(src, dst, count, opt)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Fprintf(w, "no sessions found in %s\n", filepath.Join(src, "projects"))
		return nil
	}
	fmt.Fprintf(w, "%s: %s -> %s\n", kind, src, dst)

	var done, same, skipped, failed, stripped int
	for _, r := range results {
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
		fmt.Fprintln(w, line)
	}

	fmt.Fprintf(w, "%d written, %d unchanged, %d skipped, %d failed\n", done, same, skipped, failed)
	if opt.StripThinking && stripped > 0 {
		fmt.Fprintf(w, "removed %d thinking blocks (their signatures are only valid for the provider that wrote them)\n", stripped)
	}
	if kind == "import" && done > 0 {
		fmt.Fprintf(w, "next: ccgp %s, then /resume (sessions are listed per project directory)\n", profile)
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
