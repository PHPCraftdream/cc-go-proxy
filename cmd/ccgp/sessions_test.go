package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if _, _, err := parseSessionArgs("export", nil); err == nil || !strings.Contains(err.Error(), "ccgp export") {
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
