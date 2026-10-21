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
	src, dst := t.TempDir(), t.TempDir()
	writeSession(t, src, "D--proj", "0123456789abcdef", `{"type":"user","uuid":"u"}`+"\n")

	var out bytes.Buffer
	if err := copySessions(&out, "import", "zai", src, dst, 10, sessions.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"copied", "D--proj/01234567", "1 written, 0 unchanged, 0 skipped, 0 failed", "next: ccgp zai"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := copySessions(&out, "import", "zai", src, dst, 10, sessions.Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "0 written, 1 unchanged") || strings.Contains(out.String(), "next:") {
		t.Errorf("second run:\n%s", out.String())
	}
}

func TestCopySessionsExportStripsThinking(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeSession(t, src, "p", "s1",
		`{"type":"assistant","uuid":"a","parentUuid":null,"message":{"content":[{"type":"thinking","thinking":"t","signature":"ccgp1:x"},{"type":"text","text":"hi"}]}}`+"\n")

	var out bytes.Buffer
	if err := copySessions(&out, "export", "codex", src, dst, 10, sessions.Options{StripThinking: true}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "projects", "p", "s1.jsonl"))
	if err != nil || strings.Contains(string(b), "thinking") || !strings.Contains(string(b), "hi") {
		t.Errorf("exported %q, %v", b, err)
	}
	if !strings.Contains(out.String(), "removed 1 thinking blocks") || strings.Contains(out.String(), "next:") {
		t.Errorf("output:\n%s", out.String())
	}

	// Nothing was written the second time, so nothing was removed.
	out.Reset()
	if err := copySessions(&out, "export", "codex", src, dst, 10, sessions.Options{StripThinking: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "removed") || !strings.Contains(out.String(), "0 written, 1 unchanged") {
		t.Errorf("second run:\n%s", out.String())
	}
}

func TestCopySessionsEmptyAndFailure(t *testing.T) {
	var out bytes.Buffer
	if err := copySessions(&out, "import", "zai", t.TempDir(), t.TempDir(), 10, sessions.Options{}); err != nil ||
		!strings.Contains(out.String(), "no sessions found") {
		t.Errorf("empty: %v %q", err, out.String())
	}

	// A destination that cannot be written to is reported and gives a non-zero exit.
	src, dst := t.TempDir(), t.TempDir()
	writeSession(t, src, "p", "s1", "{}\n")
	if err := os.WriteFile(filepath.Join(dst, "projects"), []byte("a file, not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err := copySessions(&out, "import", "zai", src, dst, 10, sessions.Options{})
	var ee exitError
	if !errors.As(err, &ee) || !strings.Contains(out.String(), "failed") {
		t.Errorf("err=%v out=%q", err, out.String())
	}
}
