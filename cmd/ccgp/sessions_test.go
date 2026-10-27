package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/sessions"
)

func TestParseSessionArgs(t *testing.T) {
	p, n, err := parseSessionArgs("import", []string{"zai"})
	if err != nil || p != "zai" || n != 10 {
		t.Errorf("default count = %q %d %v", p, n, err)
	}
	if p, n, err = parseSessionArgs("export", []string{"codex", "3"}); err != nil || p != "codex" || n != 3 {
		t.Errorf("explicit count = %q %d %v", p, n, err)
	}
	for _, bad := range [][]string{nil, {"a", "b", "c"}, {"a", "0"}, {"a", "-2"}, {"a", "x"}} {
		if _, _, err := parseSessionArgs("import", bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if _, _, err := parseSessionArgs("export", nil); err == nil || !strings.Contains(err.Error(), "ccgp --export") {
		t.Errorf("usage = %v", err)
	}
}

func writeSession(t *testing.T, dir, project, id, content string) {
	t.Helper()
	p := filepath.Join(dir, "projects", project, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCopySessionsReports(t *testing.T) {
	src, dst := endpoint{dir: t.TempDir(), label: "claude"}, endpoint{dir: t.TempDir(), provider: "zai", label: "zai"}
	writeSession(t, src.dir, "D--proj", "0123456789abcdef", `{"type":"user","uuid":"u"}`+"\n")

	var out bytes.Buffer
	if err := copySessions(&out, "import zai", src, dst, 10, sessions.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"copied", "D--proj/01234567", "1 written, 0 unchanged, 0 skipped, 0 failed", "next: ccgp zai"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if !strings.HasPrefix(out.String(), "import zai: "+src.dir) {
		t.Errorf("first line:\n%s", out.String())
	}

	out.Reset()
	if err := copySessions(&out, "import zai", src, dst, 10, sessions.Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "0 written, 1 unchanged") || strings.Contains(out.String(), "next:") {
		t.Errorf("second run:\n%s", out.String())
	}
}

func TestCopySessionsExportStripsThinking(t *testing.T) {
	src, dst := endpoint{dir: t.TempDir(), provider: "codex", label: "codex"}, endpoint{dir: t.TempDir(), label: "claude"}
	writeSession(t, src.dir, "p", "s1",
		`{"type":"assistant","uuid":"a","parentUuid":null,"message":{"content":[{"type":"thinking","thinking":"t","signature":"ccgp1:x"},{"type":"text","text":"hi"}]}}`+"\n")

	var out bytes.Buffer
	if err := copySessions(&out, "export codex", src, dst, 10, sessions.Options{StripThinking: true}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst.dir, "projects", "p", "s1.jsonl"))
	if err != nil || strings.Contains(string(b), "thinking") || !strings.Contains(string(b), "hi") {
		t.Errorf("exported %q, %v", b, err)
	}
	if !strings.Contains(out.String(), "removed 1 thinking blocks") || strings.Contains(out.String(), "next:") {
		t.Errorf("output:\n%s", out.String())
	}

	// Nothing was written the second time, so nothing was removed.
	out.Reset()
	if err := copySessions(&out, "export codex", src, dst, 10, sessions.Options{StripThinking: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "removed") || !strings.Contains(out.String(), "0 written, 1 unchanged") {
		t.Errorf("second run:\n%s", out.String())
	}
}

func TestCopySessionsEmptyAndFailure(t *testing.T) {
	var out bytes.Buffer
	if err := copySessions(&out, "import zai", endpoint{dir: t.TempDir(), label: "claude"}, endpoint{dir: t.TempDir(), provider: "zai", label: "zai"}, 10, sessions.Options{}); err != nil ||
		!strings.Contains(out.String(), "no sessions found") {
		t.Errorf("empty: %v %q", err, out.String())
	}

	// A destination that cannot be written to is reported and gives a non-zero exit.
	src, dst := endpoint{dir: t.TempDir(), label: "claude"}, endpoint{dir: t.TempDir(), provider: "zai", label: "zai"}
	writeSession(t, src.dir, "p", "s1", "{}\n")
	if err := os.WriteFile(filepath.Join(dst.dir, "projects"), []byte("a file, not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err := copySessions(&out, "import zai", src, dst, 10, sessions.Options{})
	var ee exitError
	if !errors.As(err, &ee) || !strings.Contains(out.String(), "failed") {
		t.Errorf("err=%v out=%q", err, out.String())
	}
}

func TestProjectDirName(t *testing.T) {
	for in, want := range map[string]string{
		`D:\dev\ktav-lang`:             "D--dev-ktav-lang",
		`C:\Users\Computer\Desktop\pc`: "C--Users-Computer-Desktop-pc",
		"/home/me/my_proj.v2":          "-home-me-my-proj-v2",
		`D:\dev\проект`:                "D--dev-------",
	} {
		if got := projectDirName(in); got != want {
			t.Errorf("projectDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCopySessionsOnlyTheGivenDirectory(t *testing.T) {
	src, dst := endpoint{dir: t.TempDir(), label: "claude"}, endpoint{dir: t.TempDir(), provider: "codex", label: "codex"}
	writeSession(t, src.dir, "D--dev-here", "aaaaaaaa1", `{"type":"user","uuid":"u"}`+"\n")
	writeSession(t, src.dir, "D--dev-other", "bbbbbbbb2", `{"type":"user","uuid":"v"}`+"\n")

	var out bytes.Buffer
	if err := copySessions(&out, "send claude -> codex", src, dst, 10, sessions.Options{Project: "D--dev-here"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"directory: D--dev-here", "[1/1] D--dev-here/aaaaaaaa", "1 written", "next: ccgp codex"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "D--dev-other") {
		t.Errorf("another directory was copied:\n%s", got)
	}

	out.Reset()
	if err := copySessions(&out, "send claude -> codex", src, dst, 10, sessions.Options{Project: "D--dev-none"}); err != nil ||
		!strings.Contains(out.String(), "no sessions found in") || !strings.Contains(out.String(), "D--dev-none") {
		t.Errorf("empty directory: %v %q", err, out.String())
	}
}

func TestHumanSize(t *testing.T) {
	tests := map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1 KB", 1536: "2 KB",
		1<<20 - 1: "1024 KB", 1 << 20: "1.0 MB", 800 << 20: "800.0 MB", 1900 << 20: "1900.0 MB",
	}
	for n, want := range tests {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("82a9eed1-d61a-48f4-b89b-fa0c60e07356"); got != "82a9eed1" {
		t.Errorf("long id = %q", got)
	}
	if got := shortID("abc"); got != "abc" {
		t.Errorf("short id = %q", got)
	}
}

func TestSkipReasonNamesTheCause(t *testing.T) {
	if got := skipReason(sessions.Ahead); !strings.Contains(got, "already has more") {
		t.Errorf("Ahead: %q", got)
	}
	if got := skipReason(sessions.Diverged); !strings.Contains(got, "not overwritten") {
		t.Errorf("Diverged: %q", got)
	}
}

// A profile without any session gets the latest session of the working
// directory from Claude, and only that one.
func TestSeedFirstSessionCopiesTheLatestOfThisDirectory(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "work")
	project := projectDirName(cwd)
	src, dst := endpoint{dir: t.TempDir(), label: "claude"}, endpoint{dir: t.TempDir(), provider: "codex", label: "codex"}
	writeSession(t, src.dir, project, "old-session", `{"type":"user","uuid":"a"}`+"\n")
	writeSession(t, src.dir, project, "new-session", `{"type":"user","uuid":"b"}`+"\n")
	writeSession(t, src.dir, "other-project", "elsewhere", `{"type":"user","uuid":"c"}`+"\n")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(src.dir, "projects", project, "old-session.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	other := time.Now().Add(time.Hour) // newest of all, but another directory
	if err := os.Chtimes(filepath.Join(src.dir, "projects", "other-project", "elsewhere.jsonl"), other, other); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	seedFirstSession(&out, src, dst, cwd)
	got, err := sessions.List(dst.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Project != project || got[0].ID != "new-session" {
		t.Fatalf("profile sessions = %+v, want only new-session of %s", got, project)
	}
	if !strings.Contains(out.String(), "first launch of codex") {
		t.Errorf("output:\n%s", out.String())
	}

	// Second launch: the profile has a session now, nothing more is copied.
	writeSession(t, src.dir, project, "newer-session", `{"type":"user","uuid":"d"}`+"\n")
	out.Reset()
	seedFirstSession(&out, src, dst, cwd)
	if got, _ = sessions.List(dst.dir); len(got) != 1 || out.Len() != 0 {
		t.Errorf("second launch: %d sessions, output %q", len(got), out.String())
	}
}

// Nothing to copy, or something already there: silent, and nothing written.
func TestSeedFirstSessionLeavesOtherCasesAlone(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "work")
	project := projectDirName(cwd)
	count := func(dir string) int {
		l, err := sessions.List(dir)
		if err != nil {
			t.Fatal(err)
		}
		return len(l)
	}

	t.Run("claude has no session for this directory", func(t *testing.T) {
		src, dst := endpoint{dir: t.TempDir(), label: "claude"}, endpoint{dir: t.TempDir(), label: "p"}
		writeSession(t, src.dir, "another", "x", "{}\n")
		var out bytes.Buffer
		seedFirstSession(&out, src, dst, cwd)
		if out.Len() != 0 || count(dst.dir) != 0 {
			t.Errorf("output %q, %d sessions", out.String(), count(dst.dir))
		}
	})
	t.Run("claude has no projects at all", func(t *testing.T) {
		src, dst := endpoint{dir: filepath.Join(t.TempDir(), "missing"), label: "claude"}, endpoint{dir: t.TempDir(), label: "p"}
		var out bytes.Buffer
		seedFirstSession(&out, src, dst, cwd)
		if out.Len() != 0 || count(dst.dir) != 0 {
			t.Errorf("output %q, %d sessions", out.String(), count(dst.dir))
		}
	})
	t.Run("the profile already has a session in another directory", func(t *testing.T) {
		src, dst := endpoint{dir: t.TempDir(), label: "claude"}, endpoint{dir: t.TempDir(), label: "p"}
		writeSession(t, src.dir, project, "x", "{}\n")
		writeSession(t, dst.dir, "elsewhere", "mine", "{}\n")
		var out bytes.Buffer
		seedFirstSession(&out, src, dst, cwd)
		if out.Len() != 0 || count(dst.dir) != 1 {
			t.Errorf("output %q, %d sessions", out.String(), count(dst.dir))
		}
	})
	t.Run("thinking blocks do not travel to another provider", func(t *testing.T) {
		src, dst := endpoint{dir: t.TempDir(), label: "claude"}, endpoint{dir: t.TempDir(), provider: "codex", label: "codex"}
		line := `{"type":"assistant","uuid":"a","message":{"role":"assistant","content":[{"type":"thinking","thinking":"secret","signature":"sig"},{"type":"text","text":"hi"}]}}` + "\n"
		writeSession(t, src.dir, project, "x", line)
		seedFirstSession(&bytes.Buffer{}, src, dst, cwd)
		b, err := os.ReadFile(filepath.Join(dst.dir, "projects", project, "x.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "thinking\":\"secret") || !strings.Contains(string(b), "hi") {
			t.Errorf("copied transcript: %s", b)
		}
	})
}
