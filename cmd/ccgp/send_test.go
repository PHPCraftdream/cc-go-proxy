package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/sessions"
)

// sendConfig has providers codex, codex2 and zai; zai and zai2 share the
// provider zai, so only that pair may keep thinking blocks.
const sendConfig = `listen.host: 127.0.0.1
listen.port: 60094

providers: [
    {
        name: codex
        type: codex
    }
    {
        name: codex2
        type: codex
    }
    {
        name: zai
        type: anthropic_compat
        base_url: https://example.invalid/anthropic
    }
]

profiles: [
    {
        name: codex
        provider: codex
    }
    {
        name: codex2
        provider: codex2
    }
    {
        name: zai
        provider: zai
    }
    {
        name: zai2
        provider: zai
    }
]
`

func sendTestConfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(path, []byte(sendConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCGP_CONFIG", path)
}

func TestParseSendArgs(t *testing.T) {
	from, to, n, err := parseSendArgs([]string{"claude", "zai"})
	if err != nil || from != "claude" || to != "zai" || n != 10 {
		t.Errorf("default = %q %q %d %v", from, to, n, err)
	}
	if from, to, n, err = parseSendArgs([]string{"zai", "claude", "3"}); err != nil || from != "zai" || to != "claude" || n != 3 {
		t.Errorf("explicit = %q %q %d %v", from, to, n, err)
	}
	for _, bad := range [][]string{nil, {"a"}, {"a", "b", "c", "d"}, {"a", "b", "0"}, {"a", "b", "-2"}, {"a", "b", "x"}} {
		if _, _, _, err := parseSendArgs(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if _, _, _, err := parseSendArgs(nil); err == nil || !strings.Contains(err.Error(), "ccgp send") {
		t.Errorf("usage = %v", err)
	}
}

func TestResolveEndpoint(t *testing.T) {
	sendTestConfig(t)
	cfg := loadTestConfig(t)
	base, official := t.TempDir(), t.TempDir()

	cl, err := resolveEndpoint(cfg, base, official, "claude")
	if err != nil || cl.dir != official || cl.provider != "" || cl.label != "claude" {
		t.Errorf("claude = %+v, %v", cl, err)
	}
	pf, err := resolveEndpoint(cfg, base, official, "zai")
	if err != nil || pf.provider != "zai" || pf.label != "zai" || pf.dir != filepath.Join(base, "zai") {
		t.Errorf("zai = %+v, %v", pf, err)
	}
	_, err = resolveEndpoint(cfg, base, official, "nope")
	if err == nil || !strings.Contains(err.Error(), "nope") ||
		!strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "claude") {
		t.Errorf("unknown = %v, want it to mention profiles and claude", err)
	}
}

func TestStripThinkingBetweenMatrix(t *testing.T) {
	sendTestConfig(t)
	cfg := loadTestConfig(t)
	base, official := t.TempDir(), t.TempDir()
	must := func(name string) endpoint {
		t.Helper()
		e, err := resolveEndpoint(cfg, base, official, name)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	codex, codex2, zai, zai2, claude := must("codex"), must("codex2"), must("zai"), must("zai2"), must("claude")

	if stripThinkingBetween(zai, zai2) {
		t.Error("profiles on the same provider must keep thinking blocks")
	}
	for _, pair := range [][2]endpoint{{codex, codex2}, {codex, zai}, {claude, codex}, {codex, claude}} {
		if !stripThinkingBetween(pair[0], pair[1]) {
			t.Errorf("%s -> %s must strip thinking blocks", pair[0].label, pair[1].label)
		}
	}
}

func TestSendSameEndpointRefused(t *testing.T) {
	sendTestConfig(t)
	// send claude claude resolves both endpoints to the user's own dir.
	if err := sessionsCmd("send", []string{"claude", "claude"}); err == nil || !strings.Contains(err.Error(), "same") {
		t.Errorf("claude claude: %v", err)
	}
}

func TestSendAliasParity(t *testing.T) {
	sendTestConfig(t)
	impFrom, impTo, impN, err := parseSendCommand("import", []string{"zai", "3"})
	if err != nil {
		t.Fatal(err)
	}
	sendFrom, sendTo, sendN, err := parseSendArgs([]string{"claude", "zai", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if impFrom != sendFrom || impTo != sendTo || impN != sendN {
		t.Errorf("import %q->%q %d != send %q->%q %d", impFrom, impTo, impN, sendFrom, sendTo, sendN)
	}
	expFrom, expTo, expN, err := parseSendCommand("export", []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	sendFrom, sendTo, sendN, err = parseSendArgs([]string{"codex", "claude"})
	if err != nil || expFrom != sendFrom || expTo != sendTo || expN != sendN {
		t.Errorf("export %q->%q %d != send %q->%q %d (%v)", expFrom, expTo, expN, sendFrom, sendTo, sendN, err)
	}

	// Resolved, an alias is the same copy as the explicit send, thinking rule included.
	cfg := loadTestConfig(t)
	base, official := t.TempDir(), t.TempDir()
	resolve := func(name string) endpoint {
		t.Helper()
		e, err := resolveEndpoint(cfg, base, official, name)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	for _, tc := range []struct {
		kind     string
		args     []string
		from, to string
	}{
		{"import", []string{"zai"}, "claude", "zai"},
		{"export", []string{"codex"}, "codex", "claude"},
	} {
		af, at, _, err := parseSendCommand(tc.kind, tc.args)
		if err != nil {
			t.Fatal(err)
		}
		if af != tc.from || at != tc.to {
			t.Errorf("%s resolves to %q -> %q, want %q -> %q", tc.kind, af, at, tc.from, tc.to)
		}
		src, dst := resolve(af), resolve(at)
		if src != resolve(tc.from) || dst != resolve(tc.to) {
			t.Errorf("%s: endpoints %+v -> %+v differ from the explicit send", tc.kind, src, dst)
		}
		if !stripThinkingBetween(src, dst) {
			t.Errorf("%s must strip thinking blocks (claude is its own provider)", tc.kind)
		}
	}
}

func TestSendCopiesBetweenProfiles(t *testing.T) {
	sendTestConfig(t)
	cfg := loadTestConfig(t)
	base := t.TempDir()
	official := filepath.Join(base, "official")
	src, err := resolveEndpoint(cfg, base, official, "zai")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := resolveEndpoint(cfg, base, official, "zai2")
	if err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"assistant","uuid":"a","message":{"content":[{"type":"thinking","thinking":"t","signature":"ccgp1:x"},{"type":"text","text":"hi"}]}}` + "\n"
	writeSession(t, src.dir, "D--proj", "aaaabbbbccccdddd", transcript)

	// Same provider: the thinking block is kept, byte for byte.
	var out bytes.Buffer
	if err := copySessions(&out, "send zai -> zai2", src, dst, 10, sessions.Options{StripThinking: stripThinkingBetween(src, dst)}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst.dir, "projects", "D--proj", "aaaabbbbccccdddd.jsonl"))
	if err != nil || string(b) != transcript {
		t.Errorf("kept copy = %q, %v; want the input unchanged", b, err)
	}
	if strings.Contains(out.String(), "removed") {
		t.Errorf("nothing was stripped:\n%s", out.String())
	}

	// Different provider (claude as destination): the thinking block goes.
	os.RemoveAll(dst.dir)
	dst2, err := resolveEndpoint(cfg, base, official, "claude")
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := copySessions(&out, "send zai -> claude", src, dst2, 10, sessions.Options{StripThinking: stripThinkingBetween(src, dst2)}); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(filepath.Join(official, "projects", "D--proj", "aaaabbbbccccdddd.jsonl"))
	if err != nil || strings.Contains(string(b), "thinking") || !strings.Contains(string(b), "hi") {
		t.Errorf("stripped copy = %q, %v", b, err)
	}
	if !strings.Contains(out.String(), "removed 1 thinking blocks") {
		t.Errorf("output:\n%s", out.String())
	}
}

// A move between two profiles of different providers strips the thinking
// blocks (their signatures belong to the account that wrote them) and
// leaves the source untouched.
func TestSendStripsBetweenProfilesOfDifferentProviders(t *testing.T) {
	sendTestConfig(t)
	cfg := loadTestConfig(t)
	base, official := t.TempDir(), t.TempDir()
	src, err := resolveEndpoint(cfg, base, official, "codex")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := resolveEndpoint(cfg, base, official, "codex2")
	if err != nil {
		t.Fatal(err)
	}
	if !stripThinkingBetween(src, dst) {
		t.Fatal("codex -> codex2 must strip thinking blocks")
	}
	transcript := `{"type":"assistant","uuid":"a","message":{"content":[{"type":"thinking","thinking":"t","signature":"ccgp1:x"},{"type":"text","text":"hi"}]}}` + "\n"
	writeSession(t, src.dir, "D--proj", "eeeeffffgggghhhh", transcript)

	var out bytes.Buffer
	if err := copySessions(&out, "send codex -> codex2", src, dst, 10, sessions.Options{StripThinking: stripThinkingBetween(src, dst)}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst.dir, "projects", "D--proj", "eeeeffffgggghhhh.jsonl"))
	if err != nil || strings.Contains(string(b), "thinking") || !strings.Contains(string(b), "hi") {
		t.Errorf("copy = %q, %v; want the text kept and the thinking block gone", b, err)
	}
	for _, want := range []string{"removed 1 thinking blocks", "next: ccgp codex2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if orig, err := os.ReadFile(filepath.Join(src.dir, "projects", "D--proj", "eeeeffffgggghhhh.jsonl")); err != nil || string(orig) != transcript {
		t.Errorf("the source was modified: %q, %v", orig, err)
	}
}

// sendEnv points the config dir, the user's home and Claude Code's own dir at
// temp dirs, and moves into a fresh working directory. It returns Claude
// Code's own dir, the profiles' base dir and the working directory's project
// folder name.
func sendEnv(t *testing.T) (official, base, project string) {
	t.Helper()
	sendTestConfig(t)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cfgRoot := filepath.Join(root, "config")
	official = filepath.Join(root, "official")
	for _, d := range []string{home, cfgRoot, official} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", cfgRoot)
	t.Setenv("XDG_CONFIG_HOME", cfgRoot)
	t.Setenv("CLAUDE_CONFIG_DIR", official)

	work := filepath.Join(root, "work", "my-proj")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	cwd, err := os.Getwd() // the canonical path Claude Code would see
	if err != nil {
		t.Fatal(err)
	}
	base, err = claudeHomeBase()
	if err != nil {
		t.Fatal(err)
	}
	return official, base, projectDirName(cwd)
}

func TestSendCmdCopiesOnlyTheWorkingDirectory(t *testing.T) {
	official, base, project := sendEnv(t)
	writeSession(t, official, project, "aaaaaaaa-here", `{"type":"user","uuid":"a"}`+"\n")
	writeSession(t, official, "D--dev-elsewhere", "bbbbbbbb-other", `{"type":"user","uuid":"b"}`+"\n")

	if err := sessionsCmd("send", []string{"claude", "codex"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	dst := filepath.Join(base, "codex", "projects")
	if _, err := os.Stat(filepath.Join(dst, project, "aaaaaaaa-here.jsonl")); err != nil {
		t.Errorf("session of the working directory not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "D--dev-elsewhere")); err == nil {
		t.Error("a session of another directory was copied")
	}
}

func TestImportExportRoundTripKeepsToTheWorkingDirectory(t *testing.T) {
	official, base, project := sendEnv(t)
	writeSession(t, official, project, "cccccccc-here", `{"type":"user","uuid":"c"}`+"\n")
	writeSession(t, official, "D--dev-elsewhere", "dddddddd-other", `{"type":"user","uuid":"d"}`+"\n")

	if err := sessionsCmd("import", []string{"codex"}); err != nil {
		t.Fatalf("import: %v", err)
	}
	// A session made in the profile for this directory, and one for another.
	prof := filepath.Join(base, "codex")
	writeSession(t, prof, project, "eeeeeeee-new", `{"type":"user","uuid":"e"}`+"\n")
	writeSession(t, prof, "D--dev-elsewhere", "ffffffff-new", `{"type":"user","uuid":"f"}`+"\n")

	if err := sessionsCmd("export", []string{"codex"}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if _, err := os.Stat(filepath.Join(official, "projects", project, "eeeeeeee-new.jsonl")); err != nil {
		t.Errorf("export did not bring the directory's session back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(official, "projects", "D--dev-elsewhere", "ffffffff-new.jsonl")); err == nil {
		t.Error("export copied a session of another directory into Claude Code")
	}
}

func TestSendCmdWithoutSessionsHere(t *testing.T) {
	official, _, _ := sendEnv(t)
	writeSession(t, official, "D--dev-elsewhere", "gggggggg-other", `{"type":"user","uuid":"g"}`+"\n")
	if err := sessionsCmd("send", []string{"claude", "codex"}); err != nil {
		t.Fatalf("send with nothing to copy here: %v", err)
	}
}
