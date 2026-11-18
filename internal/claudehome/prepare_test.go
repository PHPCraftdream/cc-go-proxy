package claudehome

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/claudehome/dirlock"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSourceDir(t *testing.T) {
	d, s := SourceDir(func(string) string { return "" }, "/home/u")
	if d != filepath.Join("/home/u", ".claude") || s != filepath.Join("/home/u", ".claude.json") {
		t.Errorf("default = %q, %q", d, s)
	}
	d, s = SourceDir(func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return "/cfg"
		}
		return ""
	}, "/home/u")
	if d != "/cfg" || s != filepath.Join("/cfg", ".claude.json") {
		t.Errorf("override = %q, %q", d, s)
	}
}

func TestSanitizeSettings(t *testing.T) {
	out, err := SanitizeSettings([]byte(`{
		"apiKeyHelper": "get-key.sh",
		"forceLoginMethod": "claudeai",
		"permissions": {"allow": ["Bash(ls)"]},
		"env": {"ANTHROPIC_BASE_URL": "http://x", "anthropic_api_key": "k", "CLAUDE_CODE_USE_BEDROCK": "1", "KEEP": "v"},
		"hooks": {"Stop": []}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"apiKeyHelper", "forceLoginMethod"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s survived", k)
		}
	}
	env := m["env"].(map[string]any)
	if len(env) != 1 || env["KEEP"] != "v" {
		t.Errorf("env = %v", env)
	}
	if m["permissions"] == nil || m["hooks"] == nil {
		t.Errorf("unrelated settings lost: %v", m)
	}
	if _, err := SanitizeSettings([]byte("not json")); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestPrepareIsolatesAndMirrors(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	state := filepath.Join(root, "src.json")

	write(t, filepath.Join(src, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"official"}}`)
	write(t, filepath.Join(src, "CLAUDE.md"), "rules")
	write(t, filepath.Join(src, "settings.json"), `{"apiKeyHelper":"x","model":"opus"}`)
	write(t, filepath.Join(src, "history.jsonl"), "secret history")
	write(t, filepath.Join(src, "skills", "s1", "SKILL.md"), "skill")
	write(t, state, `{"oauthAccount":{"emailAddress":"me@example.com"},"userID":"u","theme":"dark","mcpServers":{"a":{}},"projects":{"/p":{}}}`)

	// A credential left in the isolated dir (e.g. by /login) is wiped on launch.
	write(t, filepath.Join(dst, ".credentials.json"), "leftover")

	if err := Prepare(src, state, dst); err != nil {
		t.Fatal(err)
	}
	if HasCredentials(dst) {
		t.Fatal("credentials present in the isolated dir")
	}
	if read(t, filepath.Join(dst, "CLAUDE.md")) != "rules" {
		t.Error("CLAUDE.md not mirrored")
	}
	if s := read(t, filepath.Join(dst, "settings.json")); strings.Contains(s, "apiKeyHelper") || !strings.Contains(s, "opus") {
		t.Errorf("settings = %s", s)
	}
	if _, err := os.Stat(filepath.Join(dst, "history.jsonl")); err == nil {
		t.Error("history copied")
	}
	if read(t, filepath.Join(dst, "skills", "s1", "SKILL.md")) != "skill" {
		t.Error("skills not shared")
	}

	st := read(t, filepath.Join(dst, ".claude.json"))
	for _, bad := range []string{"oauthAccount", "me@example.com", "userID", "projects"} {
		if strings.Contains(st, bad) {
			t.Errorf("account/project data %q copied into state: %s", bad, st)
		}
	}
	for _, want := range []string{"hasCompletedOnboarding", "theme", "mcpServers"} {
		if !strings.Contains(st, want) {
			t.Errorf("state lacks %q: %s", want, st)
		}
	}
}

func TestPrepareIsIdempotentAndFollowsSource(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "CLAUDE.md"), "v1")
	if err := Prepare(src, filepath.Join(root, "none.json"), dst); err != nil {
		t.Fatal(err)
	}
	// Claude Code edits its own state; it must survive later launches.
	write(t, filepath.Join(dst, ".claude.json"), `{"hasCompletedOnboarding":true,"numStartups":3}`)
	write(t, filepath.Join(src, "CLAUDE.md"), "v2")
	if err := os.Remove(filepath.Join(src, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "keybindings.json"), "{}")
	if err := Prepare(src, filepath.Join(root, "none.json"), dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("tracked CLAUDE.md not removed: %v", err)
	}
	if read(t, filepath.Join(dst, "keybindings.json")) != "{}" {
		t.Error("keybindings.json not mirrored")
	}
	if !strings.Contains(read(t, filepath.Join(dst, ".claude.json")), "numStartups") {
		t.Error("existing state was overwritten")
	}
}

// The interface options that only .claude.json holds (leftArrowOpensAgents and
// the like) follow the user into a new profile; the account does not.
func TestPrepareCarriesInterfacePreferences(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	state := filepath.Join(root, "src.json")
	write(t, state, `{"leftArrowOpensAgents":false,"copyOnSelect":false,"diffTool":"terminal","claudeInChromeDefaultEnabled":true,"userID":"u","oauthAccount":{"emailAddress":"me@example.com"}}`)
	if err := Prepare(src, state, dst); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dst, ".claude.json"))), &got); err != nil {
		t.Fatal(err)
	}
	if got["leftArrowOpensAgents"] != false || got["copyOnSelect"] != false || got["diffTool"] != "terminal" {
		t.Errorf("preferences not carried over: %v", got)
	}
	for _, bad := range []string{"claudeInChromeDefaultEnabled", "userID", "oauthAccount"} {
		if _, ok := got[bad]; ok {
			t.Errorf("%s was carried over: %v", bad, got)
		}
	}
}

// A profile made before a preference was carried over gets it on its next
// launch; what it already has is its own, and nothing else in its state moves.
func TestPrepareAddsMissingPreferencesToAnOldProfile(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	state := filepath.Join(root, "src.json")
	write(t, state, `{"leftArrowOpensAgents":false,"copyOnSelect":false,"prStatusFooterEnabled":false}`)
	write(t, filepath.Join(dst, ".claude.json"), `{"hasCompletedOnboarding":true,"numStartups":3,"copyOnSelect":true,"projects":{"/p":{"allowedTools":[]}}}`)
	if err := Prepare(src, state, dst); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dst, ".claude.json"))), &got); err != nil {
		t.Fatal(err)
	}
	if got["leftArrowOpensAgents"] != false || got["prStatusFooterEnabled"] != false {
		t.Errorf("missing preferences not added: %v", got)
	}
	if got["copyOnSelect"] != true {
		t.Errorf("the profile's own choice was replaced: %v", got)
	}
	if got["numStartups"] != float64(3) || got["hasCompletedOnboarding"] != true || got["projects"] == nil {
		t.Errorf("the profile's state changed: %v", got)
	}

	// A second launch changes nothing.
	before := read(t, filepath.Join(dst, ".claude.json"))
	if err := Prepare(src, state, dst); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dst, ".claude.json")) != before {
		t.Error("the state file was rewritten without a change")
	}
}

// A state file that cannot be read or parsed is left alone; the launch goes on.
func TestPreferencesAreBestEffort(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	state := filepath.Join(root, "src.json")
	write(t, state, `{"leftArrowOpensAgents":false}`)
	write(t, filepath.Join(dst, ".claude.json"), "{not json")
	if err := Prepare(src, state, dst); err != nil {
		t.Fatalf("Prepare failed for a broken state file: %v", err)
	}
	if got := read(t, filepath.Join(dst, ".claude.json")); got != "{not json" {
		t.Errorf("broken state file was rewritten: %q", got)
	}
	write(t, state, "{also not json")
	write(t, filepath.Join(dst, ".claude.json"), `{"numStartups":1}`)
	if err := Prepare(src, state, dst); err != nil || read(t, filepath.Join(dst, ".claude.json")) != `{"numStartups":1}` {
		t.Errorf("a broken source changed the profile: %v", err)
	}
}

func TestPrepareRefusesUsersOwnDir(t *testing.T) {
	d := t.TempDir()
	if err := Prepare(d, filepath.Join(d, ".claude.json"), d); err == nil {
		t.Fatal("Prepare(src == dst) succeeded")
	}
}

func TestIsAuthEnv(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "anthropic_auth_token", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_VERTEX"} {
		if !IsAuthEnv(k) {
			t.Errorf("%s not treated as auth env", k)
		}
	}
	for _, k := range []string{"PATH", "ANTHROPIC_MODEL", "CLAUDE_CONFIG_DIR"} {
		if IsAuthEnv(k) {
			t.Errorf("%s wrongly treated as auth env", k)
		}
	}
}

func TestPrepareOwnMirrorsSettingsOnly(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")

	write(t, filepath.Join(src, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"official"}}`)
	write(t, filepath.Join(src, ".claude.json"), `{"oauthAccount":{"emailAddress":"me@example.com"},"userID":"u","projects":{"/p":{}}}`)
	write(t, filepath.Join(src, "CLAUDE.md"), "rules")
	write(t, filepath.Join(src, "settings.json"), `{"apiKeyHelper":"x","forceLoginOrgUUID":"org","model":"opus","env":{"ANTHROPIC_API_KEY":"k","KEEP":"v"}}`)
	write(t, filepath.Join(src, "history.jsonl"), "secret history")
	write(t, filepath.Join(src, "projects", "p", "s1.jsonl"), "{}\n")
	write(t, filepath.Join(src, "skills", "s1", "SKILL.md"), "skill")

	// A login made inside the profile is its own and survives every launch.
	write(t, filepath.Join(dst, ".credentials.json"), "own login")
	write(t, filepath.Join(dst, ".claude.json"), `{"numStartups":3}`)

	for i := 0; i < 2; i++ {
		if err := PrepareOwn(src, dst); err != nil {
			t.Fatal(err)
		}
	}
	if read(t, filepath.Join(dst, ".credentials.json")) != "own login" {
		t.Error("the profile's own login was removed")
	}
	if read(t, filepath.Join(dst, ".claude.json")) != `{"numStartups":3}` {
		t.Error("the profile's own state was touched")
	}
	if read(t, filepath.Join(dst, "CLAUDE.md")) != "rules" || read(t, filepath.Join(dst, "skills", "s1", "SKILL.md")) != "skill" {
		t.Error("shared instructions and skills not mirrored")
	}
	s := read(t, filepath.Join(dst, "settings.json"))
	for _, bad := range []string{"apiKeyHelper", "forceLoginOrgUUID", "ANTHROPIC_API_KEY"} {
		if strings.Contains(s, bad) {
			t.Errorf("settings keep %q: %s", bad, s)
		}
	}
	if !strings.Contains(s, "opus") || !strings.Contains(s, "KEEP") {
		t.Errorf("settings lost unrelated entries: %s", s)
	}
	for _, name := range []string{"history.jsonl", "projects"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err == nil {
			t.Errorf("%s was copied", name)
		}
	}
}

func TestPrepareOwnStartsWithoutState(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, ".claude.json"), `{"userID":"u","theme":"dark","mcpServers":{"a":{}}}`)
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".claude.json")); err == nil {
		t.Error("a new own profile must start without .claude.json (Claude Code makes its own)")
	}
	if HasCredentials(dst) {
		t.Error("credentials appeared in a new own profile")
	}
}

func TestPrepareOwnRefusesUsersOwnDir(t *testing.T) {
	d := t.TempDir()
	if err := PrepareOwn(d, d); err == nil {
		t.Fatal("PrepareOwn(src == dst) succeeded")
	}
}

const ownConfigDirError = "destination is the user's own Claude Code config dir"

type aliasedConfigEntry struct {
	isDir bool
	data  []byte
}

type configDirAlias struct {
	name string
	kind string
}

func TestPrepareRejectsAliasedOwnConfigDir(t *testing.T) {
	testAliasedOwnConfigDirRefusal(t, func(src, srcState, dst string) error {
		return Prepare(src, srcState, dst)
	})
}

func TestPrepareOwnRejectsAliasedOwnConfigDir(t *testing.T) {
	testAliasedOwnConfigDirRefusal(t, func(src, _ string, dst string) error {
		return PrepareOwn(src, dst)
	})
}

func testAliasedOwnConfigDirRefusal(t *testing.T, prepare func(src, srcState, dst string) error) {
	t.Helper()

	aliases := []configDirAlias{
		{name: "destination symlink", kind: "symlink"},
		{name: "source symlink", kind: "symlink"},
	}
	if runtime.GOOS == "windows" {
		aliases = append(aliases,
			configDirAlias{name: "destination junction", kind: "junction"},
			configDirAlias{name: "source junction", kind: "junction"},
		)
	}

	for _, alias := range aliases {
		t.Run(alias.name, func(t *testing.T) {
			root := t.TempDir()
			realSrc := filepath.Join(root, "real-src")
			if err := os.MkdirAll(realSrc, 0o700); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(realSrc, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"must-stay"}}`)
			write(t, filepath.Join(realSrc, "settings.json"), `{"apiKeyHelper":"must-stay","env":{"ANTHROPIC_API_KEY":"must-stay"}}`)
			write(t, filepath.Join(realSrc, ".claude.json"), `{"userID":"must-stay","oauthAccount":{"emailAddress":"must-stay@example.com"}}`)
			write(t, filepath.Join(realSrc, "CLAUDE.md"), "must-stay instructions")

			aliasPath := filepath.Join(root, "alias")
			if alias.kind == "junction" {
				createTestJunction(t, aliasPath, realSrc)
			} else {
				if err := os.Symlink(realSrc, aliasPath); err != nil {
					t.Skipf("directory symlink unavailable: %v", err)
				}
			}

			src, dst := realSrc, aliasPath
			if strings.HasPrefix(alias.name, "source ") {
				src, dst = aliasPath, realSrc
			}
			srcState := filepath.Join(src, ".claude.json")
			before := snapshotConfigDir(t, realSrc)

			err := prepare(src, srcState, dst)

			assertConfigDirUnchanged(t, before, snapshotConfigDir(t, realSrc))
			if err == nil {
				t.Errorf("aliased own config dir was accepted; want error containing %q", ownConfigDirError)
			} else if !strings.Contains(err.Error(), ownConfigDirError) {
				t.Errorf("error = %q, want it to contain %q", err, ownConfigDirError)
			}
		})
	}
}

func createTestJunction(t *testing.T, link, target string) {
	t.Helper()
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("directory junction unavailable: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

func snapshotConfigDir(t *testing.T, root string) map[string]aliasedConfigEntry {
	t.Helper()
	entries := make(map[string]aliasedConfigEntry)
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entry := aliasedConfigEntry{isDir: d.IsDir()}
		if !d.IsDir() {
			entry.data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		entries[rel] = entry
		return nil
	}); err != nil {
		t.Fatalf("snapshot config dir %q: %v", root, err)
	}
	return entries
}

func assertConfigDirUnchanged(t *testing.T, before, after map[string]aliasedConfigEntry) {
	t.Helper()
	var names []string
	for name := range before {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := before[name]
		got, ok := after[name]
		if !ok {
			t.Errorf("source entry %q was removed", name)
			continue
		}
		if want.isDir != got.isDir || !bytes.Equal(want.data, got.data) {
			t.Errorf("source entry %q changed: before=%q after=%q", name, want.data, got.data)
		}
	}
	names = names[:0]
	for name := range after {
		if _, ok := before[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		t.Errorf("unexpected source entry created: %q", name)
	}
}

func TestSeedStateCreatesAndPreservesState(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "state")
	if err := os.WriteFile(src, []byte(`{"theme":"dark","oauthAccount":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedState(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !strings.Contains(string(got), `"theme": "dark"`) || strings.Contains(string(got), "secret") {
		t.Fatalf("seed: %q, %v", got, err)
	}
	if err := os.WriteFile(dst, []byte("session state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedState(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "session state" {
		t.Fatalf("existing state: %q, %v", got, err)
	}
	assertNoStages(t, dir)
}

// The lock is held only while preparing: a profile prepared twice in a row
// does not deadlock on its own lock.
func TestPrepareReleasesTheLock(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "agents", "a.md"), "a")
	for i := 0; i < 3; i++ {
		if err := PrepareOwn(src, dst); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	unlock, err := dirlock.Lock(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

// TestPrepareProcessHelper runs only in a child test process.
func TestPrepareProcessHelper(t *testing.T) {
	if os.Getenv("CCGP_PREPARE_HELPER") != "1" {
		return
	}
	if os.Getenv("CCGP_PREPARE_PROBE") == "1" {
		dirlock.Acquire = func(f *os.File) error {
			if err := expectContendedLock(f); err != nil {
				return err
			}
			fmt.Println("contended")
			return dirlock.LockFile(f)
		}
	} else {
		fmt.Println("ready")
	}
	src, dst := os.Getenv("CCGP_PREPARE_SOURCE"), os.Getenv("CCGP_PREPARE_DEST")
	var err error
	if os.Getenv("CCGP_PREPARE_MODE") == "proxy" {
		err = Prepare(src, filepath.Join(src, ".claude.json"), dst)
	} else {
		err = PrepareOwn(src, dst)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type prepareChild struct {
	done   chan error
	stderr bytes.Buffer
}

func startPrepareChild(t *testing.T, src, dst, mode string, probe bool) *prepareChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPrepareProcessHelper$")
	cmd.Env = append(os.Environ(), "CCGP_PREPARE_HELPER=1", "CCGP_PREPARE_SOURCE="+src, "CCGP_PREPARE_DEST="+dst, "CCGP_PREPARE_MODE="+mode)
	marker := "ready"
	if probe {
		cmd.Env = append(cmd.Env, "CCGP_PREPARE_PROBE=1")
		marker = "contended"
	}
	child := &prepareChild{done: make(chan error, 1)}
	cmd.Stderr = &child.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() || scanner.Text() != marker {
			ready <- fmt.Errorf("helper did not signal ready: %v", scanner.Err())
		} else {
			ready <- nil
		}
		// Drain the test runner's output before Wait closes the pipe.
		for scanner.Scan() {
		}
		child.done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		cancel()
		// Wait is called by the reader goroutine even if the test fails.
		<-child.done
	})
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("helper did not become ready before deadline")
	}
	return child
}

func waitPrepareChild(t *testing.T, child *prepareChild, wantError bool) {
	t.Helper()
	select {
	case err := <-child.done:
		// Leave the result for cleanup, which also guarantees the child was reaped.
		child.done <- err
		if wantError {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(child.stderr.String(), "invalid JSON") {
				t.Fatalf("expected invalid-settings error, got %v, stderr = %s", err, child.stderr.String())
			}
		} else if err != nil {
			t.Fatalf("helper error = %v, stderr = %s", err, child.stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("preparation exceeded deadline")
	}
}

func TestPrepareProcessesLockPerDestination(t *testing.T) {
	for _, mode := range []string{"proxy", "own"} {
		t.Run(mode, func(t *testing.T) { testPrepareProcessesLockPerDestination(t, mode) })
	}
}

func testPrepareProcessesLockPerDestination(t *testing.T, mode string) {
	t.Helper()
	root := t.TempDir()
	src, first, second := filepath.Join(root, "src"), filepath.Join(root, "first"), filepath.Join(root, "second")
	write(t, filepath.Join(src, "CLAUDE.md"), "shared")
	unlock, err := dirlock.Lock(src, first)
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	same := startPrepareChild(t, src, first, mode, true)
	other := startPrepareChild(t, src, second, "own", false)
	waitPrepareChild(t, other, false)
	unlock()
	locked = false
	waitPrepareChild(t, same, false)
	for _, dst := range []string{first, second} {
		if got := read(t, filepath.Join(dst, "CLAUDE.md")); got != "shared" {
			t.Fatalf("%s: got %q", dst, got)
		}
	}
}

func TestPrepareProcessRetryPreservesLocalData(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "CLAUDE.md"), "source")
	write(t, filepath.Join(src, "settings.json"), "invalid")
	write(t, filepath.Join(src, credentialsFile), "source login")
	write(t, filepath.Join(dst, credentialsFile), "own login")
	write(t, filepath.Join(dst, "projects", "session.jsonl"), "local session")
	waitPrepareChild(t, startPrepareChild(t, src, dst, "own", false), true)
	write(t, filepath.Join(src, "settings.json"), `{}`)
	waitPrepareChild(t, startPrepareChild(t, src, dst, "own", false), false)
	for path, want := range map[string]string{
		filepath.Join(src, "CLAUDE.md"):                 "source",
		filepath.Join(src, credentialsFile):             "source login",
		filepath.Join(dst, credentialsFile):             "own login",
		filepath.Join(dst, "projects", "session.jsonl"): "local session",
		filepath.Join(dst, "CLAUDE.md"):                 "source",
	} {
		if got := read(t, path); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}

// expectContendedLock fails unless another process holds the destination lock.
func expectContendedLock(f *os.File) error {
	held, err := dirlock.Held(f)
	if err != nil {
		return fmt.Errorf("probe destination lock: %w", err)
	}
	if !held {
		return fmt.Errorf("destination lock unexpectedly available")
	}
	return nil
}

func assertNoStages(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".ccgp-stage-") {
			t.Fatalf("staged file left behind: %s", entry.Name())
		}
	}
}
