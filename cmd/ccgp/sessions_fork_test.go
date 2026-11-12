package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/sessions"
)

const forkTestID = "aaaaaaaa-1111-4222-8333-444444444444"

func forkLine(id, text string) string {
	return `{"type":"user","sessionId":"` + id + `","message":{"content":"` + text + `"}}` + "\n"
}

func TestSplitSessionFlags(t *testing.T) {
	for _, tc := range []struct {
		args        []string
		rest        []string
		fork, force bool
	}{
		{[]string{"zai"}, []string{"zai"}, false, false},
		{[]string{"zai", "3"}, []string{"zai", "3"}, false, false},
		{[]string{"zai", "--fork"}, []string{"zai"}, true, false},
		{[]string{"--fork", "zai", "3"}, []string{"zai", "3"}, true, false},
		{[]string{"claude", "--force", "codex", "2"}, []string{"claude", "codex", "2"}, false, true},
		{[]string{"claude", "codex", "--force"}, []string{"claude", "codex"}, false, true},
		{nil, nil, false, false},
	} {
		rest, fork, force, err := splitSessionFlags(tc.args)
		if err != nil || !reflect.DeepEqual(rest, tc.rest) || fork != tc.fork || force != tc.force {
			t.Errorf("%v -> %v fork=%v force=%v err=%v; want %v fork=%v force=%v", tc.args, rest, fork, force, err, tc.rest, tc.fork, tc.force)
		}
	}
	if _, _, _, err := splitSessionFlags([]string{"zai", "--fork", "--force"}); err == nil || !strings.Contains(err.Error(), "exclude each other") {
		t.Errorf("both flags: %v", err)
	}
}

// The flags may stand anywhere, and the rest of the line is read as before.
func TestSessionCommandsTakeTheFlagsAnywhere(t *testing.T) {
	for _, tc := range []struct {
		kind string
		args []string
		from string
		to   string
		n    int
	}{
		{"import", []string{"codex", "--fork", "3"}, "claude", "codex", 3},
		{"export", []string{"--force", "codex"}, "codex", "claude", 10},
		{"send", []string{"claude", "--fork", "codex", "2"}, "claude", "codex", 2},
	} {
		rest, _, _, err := splitSessionFlags(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		from, to, n, err := parseSendCommand(tc.kind, rest)
		if err != nil || from != tc.from || to != tc.to || n != tc.n {
			t.Errorf("%s %v -> %q %q %d %v; want %q %q %d", tc.kind, tc.args, from, to, n, err, tc.from, tc.to, tc.n)
		}
	}
	for _, kind := range []string{"import", "export", "send"} {
		if err := sessionsCmd(kind, []string{"a", "b", "--fork", "--force"}); err == nil || !strings.Contains(err.Error(), "exclude each other") {
			t.Errorf("%s with both flags: %v", kind, err)
		}
	}
}

// diverge has the session on both sides, gone on differently.
func diverge(t *testing.T) (src, dst endpoint) {
	t.Helper()
	src = endpoint{dir: t.TempDir(), label: "claude"}
	dst = endpoint{dir: t.TempDir(), provider: "zai", label: "zai"}
	writeSession(t, src.dir, "D--proj", forkTestID, forkLine(forkTestID, "one")+forkLine(forkTestID, "from-claude"))
	writeSession(t, dst.dir, "D--proj", forkTestID, forkLine(forkTestID, "one")+forkLine(forkTestID, "from-zai"))
	return src, dst
}

func report(t *testing.T, src, dst endpoint, opt sessions.Options) string {
	t.Helper()
	var out bytes.Buffer
	if err := copySessions(&out, "import zai", src, dst, 10, opt); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestCopySessionsSaysHowToGetPastADivergedSession(t *testing.T) {
	src, dst := diverge(t)
	out := report(t, src, dst, sessions.Options{})
	for _, want := range []string{"diverged", "not overwritten", "0 written, 0 unchanged, 1 skipped", "diverged sessions: --fork", "--force"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// A destination that is only ahead is no divergence: no hint to fork.
	src2, dst2 := endpoint{dir: t.TempDir(), label: "claude"}, endpoint{dir: t.TempDir(), provider: "zai", label: "zai"}
	writeSession(t, src2.dir, "D--proj", forkTestID, forkLine(forkTestID, "one"))
	writeSession(t, dst2.dir, "D--proj", forkTestID, forkLine(forkTestID, "one")+forkLine(forkTestID, "more"))
	out = report(t, src2, dst2, sessions.Options{})
	if !strings.Contains(out, "ahead") || strings.Contains(out, "diverged sessions:") {
		t.Errorf("ahead:\n%s", out)
	}
}

func TestCopySessionsForkReport(t *testing.T) {
	src, dst := diverge(t)
	out := report(t, src, dst, sessions.Options{Fork: true})
	m := regexp.MustCompile(`new session ([0-9a-f-]{36})\)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the line does not name the new session:\n%s", out)
	}
	for _, want := range []string{"forked", "both versions kept", "1 written, 0 unchanged, 0 skipped", "open a fork with: ccgp zai --resume " + m[1]} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "diverged sessions:") {
		t.Errorf("hint after the choice was made:\n%s", out)
	}

	// The same again: the fork is there already, nothing is written.
	out = report(t, src, dst, sessions.Options{Fork: true})
	if !strings.Contains(out, "0 written, 1 unchanged") || !strings.Contains(out, "as the session "+m[1]) || !strings.Contains(out, "--resume "+m[1]) {
		t.Errorf("second run:\n%s", out)
	}

	// The fork went its own way too: say so, and name the way out.
	fork := filepath.Join(dst.dir, "projects", "D--proj", m[1]+".jsonl")
	if err := os.WriteFile(fork, []byte(forkLine(m[1], "one")+forkLine(m[1], "else")), 0o644); err != nil {
		t.Fatal(err)
	}
	out = report(t, src, dst, sessions.Options{Fork: true})
	if !strings.Contains(out, "0 written, 0 unchanged, 1 skipped") || !strings.Contains(out, "the fork "+m[1]+" went its own way too and was left alone") {
		t.Errorf("fork diverged:\n%s", out)
	}
}

func TestCopySessionsForkIntoClaudeSaysHowToOpenIt(t *testing.T) {
	src, dst := diverge(t)
	src, dst = dst, src // zai -> claude: the destination is the user's own Claude Code
	out := report(t, src, dst, sessions.Options{Fork: true})
	m := regexp.MustCompile(`new session ([0-9a-f-]{36})\)`).FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "forked") || strings.Contains(out, "open a fork with: ccgp") ||
		!strings.Contains(out, "open it with: claude --resume "+m[1]) {
		t.Errorf("output:\n%s", out)
	}
}

func TestCopySessionsForceReport(t *testing.T) {
	src, dst := diverge(t)
	out := report(t, src, dst, sessions.Options{Force: true})
	for _, want := range []string{"replaced", "the old one is kept as ", ".jsonl.bak-", "1 written, 0 unchanged, 0 skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	got, err := os.ReadFile(filepath.Join(dst.dir, "projects", "D--proj", forkTestID+".jsonl"))
	if err != nil || string(got) != forkLine(forkTestID, "one")+forkLine(forkTestID, "from-claude") {
		t.Errorf("destination = %q, %v", got, err)
	}
	if strings.Contains(out, "diverged sessions:") || strings.Contains(out, "open a fork") {
		t.Errorf("output:\n%s", out)
	}
}

// ---- through the command ----

func TestSessionsCmdForkAndForce(t *testing.T) {
	official, base, project := sendEnv(t)
	prof := filepath.Join(base, "codex")
	writeSession(t, official, project, forkTestID, forkLine(forkTestID, "one")+forkLine(forkTestID, "from-claude"))
	if err := sessionsCmd("import", []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	// The profile goes on by itself; so does Claude Code.
	writeSession(t, prof, project, forkTestID, forkLine(forkTestID, "one")+forkLine(forkTestID, "from-codex"))

	list := func() []sessions.Session {
		t.Helper()
		l, err := sessions.List(prof)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	profileFile := filepath.Join(prof, "projects", project, forkTestID+".jsonl")
	mine := forkLine(forkTestID, "one") + forkLine(forkTestID, "from-codex")

	// Without a flag nothing changes and the command still succeeds.
	if err := sessionsCmd("import", []string{"codex"}); err != nil {
		t.Fatalf("diverged without flags: %v", err)
	}
	if l := list(); len(l) != 1 {
		t.Fatalf("sessions = %d, want 1", len(l))
	}

	// --fork: a second session next to it, the first one as it was.
	if err := sessionsCmd("import", []string{"codex", "--fork"}); err != nil {
		t.Fatal(err)
	}
	l := list()
	if len(l) != 2 {
		t.Fatalf("sessions = %d, want the old one and the fork", len(l))
	}
	if got, _ := os.ReadFile(profileFile); string(got) != mine {
		t.Errorf("the profile's session changed: %q", got)
	}
	var fork sessions.Session
	for _, s := range l {
		if s.ID != forkTestID {
			fork = s
		}
	}
	body, err := os.ReadFile(fork.Path)
	if err != nil || strings.Contains(string(body), forkTestID) || !strings.Contains(string(body), fork.ID) || !strings.Contains(string(body), "from-claude") {
		t.Errorf("fork = %q, %v", body, err)
	}

	// --force: the profile's session becomes Claude Code's, the old one is kept.
	if err := sessionsCmd("import", []string{"codex", "1", "--force"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(profileFile); string(got) != forkLine(forkTestID, "one")+forkLine(forkTestID, "from-claude") {
		t.Errorf("after force = %q", got)
	}
	baks, _ := filepath.Glob(profileFile + ".bak-*")
	if len(baks) != 1 {
		t.Fatalf("backups = %v", baks)
	}
	if got, _ := os.ReadFile(baks[0]); string(got) != mine {
		t.Errorf("backup = %q, want what the profile had", got)
	}
}

// The same id in another folder of the destination: not written, and the
// report names the folder and --fork; with --fork it goes under a new id that
// the report tells how to open in Claude Code.
func TestCopySessionsIdInAnotherFolder(t *testing.T) {
	src := endpoint{dir: t.TempDir(), provider: "codex", label: "codex"}
	dst := endpoint{dir: t.TempDir(), label: "claude"}
	writeSession(t, src.dir, "D--proj-worktrees-x", forkTestID, forkLine(forkTestID, "one")+forkLine(forkTestID, "new"))
	writeSession(t, dst.dir, "D--proj", forkTestID, forkLine(forkTestID, "one")+forkLine(forkTestID, "old"))

	out := report(t, src, dst, sessions.Options{})
	for _, want := range []string{"elsewhere", "has a session of this id in D--proj", "0 written, 0 unchanged, 1 skipped",
		"--fork writes it under a new id"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	out = report(t, src, dst, sessions.Options{Fork: true})
	m := regexp.MustCompile(`new session ([0-9a-f-]{36})\)`).FindStringSubmatch(out)
	if m == nil || m[1] == forkTestID {
		t.Fatalf("no new session named:\n%s", out)
	}
	for _, want := range []string{"forked", "1 written", "open it with: claude --resume " + m[1]} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--fork writes it") {
		t.Errorf("hint after the choice was made:\n%s", out)
	}
}
