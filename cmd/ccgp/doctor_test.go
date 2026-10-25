package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/daemon"
	"github.com/PHPCraftdream/cc-go-proxy/internal/ktavfile"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

// fakeDoctorEnv returns an all-ok doctor env that touches no real state.
func fakeDoctorEnv() *doctorEnv {
	return &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/claude", nil },
		loadConfig: func() (*config.Config, error) {
			return &config.Config{ConfigFile: "x", Providers: map[string]*config.Provider{}}, nil
		},
		defaultMiss:  func() bool { return false },
		daemonCheck:  func(string) (*daemon.Health, error) { return nil, errors.New("not running") },
		base:         "http://127.0.0.1:8080",
		ownVersion:   "test",
		ownExe:       "/bin/ccgp",
		ownExeMod:    1,
		store:        creds.Store{Dir: "D"},
		loginStateFn: func(creds.Store, *config.Provider) string { return "API key saved" },
		refreshAge:   func(creds.Store, *config.Provider) (time.Time, bool) { return time.Time{}, false },
		probe:        func(string) (int, error) { return 200, nil },
		stat:         func(string) (fs.FileInfo, error) { return fakeFileInfo{mode: 0o700}, nil },
		windows:      false,
		logPath:      func() (string, error) { return filepath.Join("L", "server.log"), nil },
		logWritable:  func(string) error { return nil },
		now:          func() time.Time { return time.Unix(1000000000, 0) },
	}
}

// fakeFileInfo is a minimal fs.FileInfo with a fixed mode.
type fakeFileInfo struct {
	mode fs.FileMode
}

func (f fakeFileInfo) Name() string       { return "d" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

func TestDoctorClaudeCheck(t *testing.T) {
	env := fakeDoctorEnv()
	if c := checkClaude(env); c.Status != "ok" {
		t.Fatalf("claude found: status %q, detail %q", c.Status, c.Detail)
	}
	env.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	c := checkClaude(env)
	if c.Status != "fail" || !bytes.Contains([]byte(c.Detail), []byte("install Claude Code")) {
		t.Fatalf("claude missing: status %q, detail %q", c.Status, c.Detail)
	}
}

func TestDoctorConfigCheck(t *testing.T) {
	env := fakeDoctorEnv()
	env.defaultMiss = func() bool { return true }
	env.loadConfig = func() (*config.Config, error) { return nil, errors.New("bad config") }
	if c := checkConfig(env); c.Status != "warn" || !bytes.Contains([]byte(c.Detail), []byte("not created yet")) {
		t.Fatalf("missing + load error: status %q, detail %q", c.Status, c.Detail)
	}
	env = fakeDoctorEnv()
	env.defaultMiss = func() bool { return true }
	env.loadConfig = func() (*config.Config, error) { return &config.Config{}, nil }
	if c := checkConfig(env); c.Status != "warn" || !bytes.Contains([]byte(c.Detail), []byte("not created yet")) {
		t.Fatalf("missing + env-only config: status %q, detail %q", c.Status, c.Detail)
	}
	env = fakeDoctorEnv()
	env.loadConfig = func() (*config.Config, error) { return nil, errors.New("bad config") }
	if c := checkConfig(env); c.Status != "fail" {
		t.Fatalf("load error: status %q", c.Status)
	}
	env = fakeDoctorEnv()
	env.loadConfig = func() (*config.Config, error) { return &config.Config{}, nil }
	if c := checkConfig(env); c.Status != "ok" || !bytes.Contains([]byte(c.Detail), []byte("env-only mode")) {
		t.Fatalf("env mode: status %q, detail %q", c.Status, c.Detail)
	}
	env = fakeDoctorEnv()
	env.defaultMiss = func() bool { return true }
	if c := checkConfig(env); c.Status != "warn" || !bytes.Contains([]byte(c.Detail), []byte("not created yet")) {
		t.Fatalf("missing config: status %q, detail %q", c.Status, c.Detail)
	}
	env = fakeDoctorEnv()
	if c := checkConfig(env); c.Status != "ok" || c.Detail != "x" {
		t.Fatalf("normal config: status %q, detail %q", c.Status, c.Detail)
	}
}

func TestDoctorServerCheck(t *testing.T) {
	env := fakeDoctorEnv()
	if c := checkServer(env); c.Status != "ok" || !bytes.Contains([]byte(c.Detail), []byte("not running (starts on demand)")) {
		t.Fatalf("down: status %q, detail %q", c.Status, c.Detail)
	}
	env.daemonCheck = func(string) (*daemon.Health, error) {
		return &daemon.Health{Version: env.ownVersion, PID: 42}, nil
	}
	c := checkServer(env)
	if c.Status != "ok" || !bytes.Contains([]byte(c.Detail), []byte("pid 42")) {
		t.Fatalf("running: status %q, detail %q", c.Status, c.Detail)
	}
	env.daemonCheck = func(string) (*daemon.Health, error) {
		return &daemon.Health{Version: "old", PID: 7}, nil
	}
	if c := checkServer(env); c.Status != "warn" || !bytes.Contains([]byte(c.Detail), []byte("ccgp --restart")) {
		t.Fatalf("stale: status %q, detail %q", c.Status, c.Detail)
	}
}

func TestDoctorLoginChecks(t *testing.T) {
	now := time.Unix(1000000000, 0)
	env := fakeDoctorEnv()
	env.now = func() time.Time { return now }
	env.store = creds.Store{Dir: "D"}
	cfg := &config.Config{ConfigFile: "x", Providers: map[string]*config.Provider{
		"b": {Name: "b", Type: config.ProviderTypeCodexAPI},
		"a": {Name: "a", Type: config.ProviderTypeCodex},
	}}
	// a is codex: report old tokens when asked.
	env.refreshAge = func(_ creds.Store, pr *config.Provider) (time.Time, bool) {
		if pr.Name == "a" {
			return now.Add(-9 * 24 * time.Hour), true
		}
		return time.Time{}, false
	}
	env.loginStateFn = func(_ creds.Store, pr *config.Provider) string {
		if pr.Name == "a" {
			return "logged in, tokens refreshed sometime"
		}
		return "no API key (ccgp --login b)"
	}
	checks := checkLogins(env, cfg)
	if len(checks) != 2 {
		t.Fatalf("want 2 checks, got %d: %+v", len(checks), checks)
	}
	if checks[0].Name != "login a" || checks[1].Name != "login b" {
		t.Fatalf("not sorted: %q, %q", checks[0].Name, checks[1].Name)
	}
	if checks[1].Status != "warn" || !bytes.Contains([]byte(checks[1].Detail), []byte("no API key")) {
		t.Fatalf("no key: status %q, detail %q", checks[1].Status, checks[1].Detail)
	}
	if checks[0].Status != "warn" || !bytes.Contains([]byte(checks[0].Detail), []byte("days old")) || !bytes.Contains([]byte(checks[0].Detail), []byte("ccgp --login")) {
		t.Fatalf("old tokens: status %q, detail %q", checks[0].Status, checks[0].Detail)
	}
	// Fresh tokens stay ok.
	env.refreshAge = func(creds.Store, *config.Provider) (time.Time, bool) { return now.Add(-24 * time.Hour), true }
	checks = checkLogins(env, cfg)
	if checks[0].Status != "ok" {
		t.Fatalf("fresh tokens: status %q, detail %q", checks[0].Status, checks[0].Detail)
	}
	// "API key saved" is ok.
	cfg2 := &config.Config{Providers: map[string]*config.Provider{"k": {Name: "k", Type: config.ProviderTypeCodexAPI}}}
	env.loginStateFn = func(creds.Store, *config.Provider) string { return "API key saved" }
	if checks := checkLogins(env, cfg2); checks[0].Status != "ok" {
		t.Fatalf("api key saved: status %q", checks[0].Status)
	}
}

func TestDoctorBackendChecks(t *testing.T) {
	env := fakeDoctorEnv()
	env.probe = func(string) (int, error) { return 404, nil }
	cfg := &config.Config{Providers: map[string]*config.Provider{
		"p": {Name: "p", Type: config.ProviderTypeCodexAPI, BaseURL: "http://example"},
	}}
	checks := checkBackends(env, cfg)
	if len(checks) != 1 || checks[0].Status != "ok" || !bytes.Contains([]byte(checks[0].Detail), []byte("reachable (HTTP 404)")) {
		t.Fatalf("404: %+v", checks)
	}
	env.probe = func(string) (int, error) { return 0, errors.New("boom") }
	checks = checkBackends(env, cfg)
	if checks[0].Status != "warn" || !bytes.Contains([]byte(checks[0].Detail), []byte("unreachable")) {
		t.Fatalf("unreachable: %+v", checks)
	}
	// codex without base URL probes codex.DefaultBaseURL.
	var probed string
	env.probe = func(u string) (int, error) { probed = u; return 200, nil }
	cfg = &config.Config{Providers: map[string]*config.Provider{
		"c":  {Name: "c", Type: config.ProviderTypeCodex},
		"no": {Name: "no", Type: config.ProviderTypeCodexAPI},
	}}
	checks = checkBackends(env, cfg)
	if len(checks) != 1 {
		t.Fatalf("want 1 check (empty non-codex URL skipped), got %+v", checks)
	}
	if probed != codex.DefaultBaseURL {
		t.Fatalf("probed %q, want %q", probed, codex.DefaultBaseURL)
	}
	// offline skips backend checks and never probes; online probes once.
	calls := 0
	env.probe = func(string) (int, error) { calls++; return 200, nil }
	cfg = &config.Config{ConfigFile: "x", Providers: map[string]*config.Provider{
		"p": {Name: "p", Type: config.ProviderTypeCodexAPI, BaseURL: "http://example"},
	}}
	checks = doctorRun(env, cfg, true)
	if calls != 0 {
		t.Fatalf("offline probed %d times", calls)
	}
	for _, c := range checks {
		if c.Name == "backend p" {
			t.Fatalf("offline run has backend check: %+v", checks)
		}
	}
	var got []string
	for _, c := range checks {
		got = append(got, c.Name)
	}
	if want := "claude,config,server,login p,creds dir,log file"; strings.Join(got, ",") != want {
		t.Fatalf("offline checks = %q, want %q", strings.Join(got, ","), want)
	}
	doctorRun(env, cfg, false)
	if calls != 1 {
		t.Fatalf("online probed %d times, want 1", calls)
	}
}

func TestDoctorCredsDir(t *testing.T) {
	env := fakeDoctorEnv()
	env.stat = func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
	if c := checkCredsDir(env); c.Status != "ok" || !bytes.Contains([]byte(c.Detail), []byte("not created yet")) {
		t.Fatalf("missing: %+v", c)
	}
	env = fakeDoctorEnv()
	env.windows = true
	if c := checkCredsDir(env); c.Status != "ok" || !bytes.Contains([]byte(c.Detail), []byte("not checked")) {
		t.Fatalf("windows: %+v", c)
	}
	env = fakeDoctorEnv()
	env.stat = func(string) (fs.FileInfo, error) { return fakeFileInfo{mode: 0o755}, nil }
	c := checkCredsDir(env)
	if c.Status != "warn" || !bytes.Contains([]byte(c.Detail), []byte("chmod 700")) {
		t.Fatalf("0755: %+v", c)
	}
	env.stat = func(string) (fs.FileInfo, error) { return fakeFileInfo{mode: 0o700}, nil }
	if c := checkCredsDir(env); c.Status != "ok" {
		t.Fatalf("0700: %+v", c)
	}
	env.stat = func(string) (fs.FileInfo, error) { return nil, errors.New("io error") }
	if c := checkCredsDir(env); c.Status != "warn" {
		t.Fatalf("stat error: %+v", c)
	}
	env = fakeDoctorEnv()
	env.store = creds.Store{}
	if c := checkCredsDir(env); c.Status != "warn" || !bytes.Contains([]byte(c.Detail), []byte("cannot determine")) {
		t.Fatalf("empty dir: %+v", c)
	}
}

func TestDoctorLogFile(t *testing.T) {
	env := fakeDoctorEnv()
	env.logPath = func() (string, error) { return "", errors.New("no log path") }
	if c := checkLogFile(env); c.Status != "warn" {
		t.Fatalf("logPath error: %+v", c)
	}
	env = fakeDoctorEnv()
	env.logWritable = func(string) error { return errors.New("denied") }
	c := checkLogFile(env)
	if c.Status != "warn" || !bytes.Contains([]byte(c.Detail), []byte("not writable")) {
		t.Fatalf("unwritable: %+v", c)
	}
	env = fakeDoctorEnv()
	c = checkLogFile(env)
	if c.Status != "ok" || c.Detail != filepath.Join("L", "server.log") {
		t.Fatalf("ok: %+v", c)
	}
	// Real writability: a temp dir is writable, a path under a regular file is not.
	if err := defaultLogWritable(t.TempDir()); err != nil {
		t.Fatalf("temp dir not writable: %v", err)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := defaultLogWritable(filepath.Join(f, "sub")); err == nil {
		t.Fatal("dir under regular file: want error")
	}
}

func TestDoctorRunnerOrderAndExit(t *testing.T) {
	env := fakeDoctorEnv()
	cfg, _ := env.loadConfig()
	checks := doctorRun(env, cfg, false)
	want := []string{"claude", "config", "server", "creds dir", "log file"}
	if len(checks) != len(want) {
		t.Fatalf("want %d checks, got %+v", len(want), checks)
	}
	for i, w := range want {
		if checks[i].Name != w {
			t.Fatalf("check %d: name %q, want %q", i, checks[i].Name, w)
		}
	}
	// With providers: all logins first (sorted), then all backends (sorted).
	env = fakeDoctorEnv()
	cfg = &config.Config{ConfigFile: "x", Providers: map[string]*config.Provider{
		"b": {Name: "b", Type: config.ProviderTypeCodexAPI, BaseURL: "http://example"},
		"a": {Name: "a", Type: config.ProviderTypeCodexAPI, BaseURL: "http://example"},
	}}
	checks = doctorRun(env, cfg, false)
	wantProviders := []string{"claude", "config", "server", "login a", "login b", "backend a", "backend b", "creds dir", "log file"}
	if len(checks) != len(wantProviders) {
		t.Fatalf("want %d checks, got %+v", len(wantProviders), checks)
	}
	for i, w := range wantProviders {
		if checks[i].Name != w {
			t.Fatalf("check %d: name %q, want %q", i, checks[i].Name, w)
		}
	}
	// Failing claude check exits with 1 after printing.
	env.lookPath = func(string) (string, error) { return "", errors.New("nope") }
	var buf bytes.Buffer
	err := runDoctor(nil, &buf, env)
	var ee exitError
	if !errors.As(err, &ee) || ee != 1 {
		t.Fatalf("want exitError(1), got %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("[FAIL]")) {
		t.Fatalf("output lacks [FAIL]: %q", buf.String())
	}
	// Warnings only: nil error, aligned text lines.
	env = fakeDoctorEnv()
	env.defaultMiss = func() bool { return true }
	buf.Reset()
	if err := runDoctor(nil, &buf, env); err != nil {
		t.Fatalf("warn-only: %v", err)
	}
	out := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("[warn]")) || !bytes.Contains(buf.Bytes(), []byte("[ok]   claude")) {
		t.Fatalf("bad text output: %q", out)
	}
}

func TestDoctorJSON(t *testing.T) {
	env := fakeDoctorEnv()
	var buf bytes.Buffer
	if err := runDoctor([]string{"--json"}, &buf, env); err != nil {
		t.Fatalf("ok run: %v", err)
	}
	var rep struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, buf.String())
	}
	if !rep.OK || len(rep.Checks) == 0 || rep.Checks[0].Name != "claude" || rep.Checks[0].Status != "ok" || rep.Checks[0].Detail != "found in PATH" {
		t.Fatalf("bad report: %+v", rep)
	}
	for _, c := range rep.Checks {
		if c.Status != "ok" {
			t.Fatalf("want only ok statuses, got %+v", rep)
		}
	}
	env.lookPath = func(string) (string, error) { return "", errors.New("nope") }
	buf.Reset()
	err := runDoctor([]string{"--json"}, &buf, env)
	var ee exitError
	if !errors.As(err, &ee) || ee != 1 {
		t.Fatalf("want exitError(1), got %v", err)
	}
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rep.OK {
		t.Fatal("OK should be false")
	}
	found := false
	for _, c := range rep.Checks {
		if c.Status == "fail" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no fail check: %+v", rep)
	}
	for _, c := range rep.Checks {
		if c.Name == "claude" && !bytes.Contains([]byte(c.Detail), []byte("install Claude Code")) {
			t.Fatalf("failing claude detail: %q", c.Detail)
		}
	}
}

func TestDoctorDefaultEnvFuncsSet(t *testing.T) {
	env := defaultDoctorEnv()
	v := reflect.ValueOf(*env)
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Type.Kind() == reflect.Func && v.Field(i).IsNil() {
			t.Errorf("defaultDoctorEnv: func field %s is nil", typ.Field(i).Name)
		}
	}
}

func TestDoctorRefreshAgeRealStore(t *testing.T) {
	now := time.Unix(1000000000, 0)
	dir := t.TempDir()
	st := creds.Store{Dir: dir}
	pr := &config.Provider{Name: "codex", Type: config.ProviderTypeCodex}

	// Fresh tokens: ok.
	p := filepath.Join(dir, "oauth-codex"+ktavfile.Ext)
	if err := ktavfile.Write(p, &oauth.Tokens{AccessToken: "a", RefreshToken: "r", LastRefresh: now}); err != nil {
		t.Fatalf("write tokens: %v", err)
	}
	last, ok := oauthRefreshAge(st, pr)
	if !ok || !last.Equal(now) {
		t.Fatalf("fresh: ok=%v last=%v want %v", ok, last, now)
	}

	// Empty dir: no file, no panic, ok=false.
	if _, ok := oauthRefreshAge(creds.Store{Dir: t.TempDir()}, pr); ok {
		t.Fatal("empty dir: want ok=false")
	}

	// Non-codex provider: ok=false even with a file.
	if _, ok := oauthRefreshAge(st, &config.Provider{Name: "codex", Type: config.ProviderTypeCodexAPI}); ok {
		t.Fatal("non-codex: want ok=false")
	}

	// Full checkLogins through the real refreshAge wiring: fresh -> ok.
	writeTokens := func(age time.Duration) {
		if err := ktavfile.Write(p, &oauth.Tokens{AccessToken: "a", RefreshToken: "r", LastRefresh: now.Add(age)}); err != nil {
			t.Fatalf("write tokens: %v", err)
		}
	}
	cfg := &config.Config{ConfigFile: "x", Providers: map[string]*config.Provider{"codex": pr}}
	env := fakeDoctorEnv()
	env.now = func() time.Time { return now }
	env.store = st
	env.refreshAge = oauthRefreshAge
	env.loginStateFn = func(creds.Store, *config.Provider) string { return "logged in, tokens refreshed sometime" }
	writeTokens(0)
	if checks := checkLogins(env, cfg); checks[0].Status != "ok" {
		t.Fatalf("fresh tokens via real store: %+v", checks)
	}

	// 9-day-old tokens: warn with the refresh hint.
	writeTokens(-9 * 24 * time.Hour)
	checks := checkLogins(env, cfg)
	if checks[0].Status != "warn" || !bytes.Contains([]byte(checks[0].Detail), []byte("days old")) || !bytes.Contains([]byte(checks[0].Detail), []byte("ccgp --login")) {
		t.Fatalf("old tokens via real store: %+v", checks)
	}
}

func TestDoctorArgs(t *testing.T) {
	err := runDoctor([]string{"--bogus"}, &bytes.Buffer{}, fakeDoctorEnv())
	if err == nil || err.Error() != "usage: ccgp --doctor [--offline] [--json]" {
		t.Fatalf("bogus arg: %v", err)
	}
	for _, args := range [][]string{{"--offline"}, {"--json"}, {"--offline", "--json"}} {
		if err := runDoctor(args, &bytes.Buffer{}, fakeDoctorEnv()); err != nil {
			t.Fatalf("args %v: %v", args, err)
		}
	}
}

func TestProbeURLReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	code, err := probeURL(srv.URL)
	if err != nil || code != http.StatusNotFound {
		t.Fatalf("probe: code %d, err %v", code, err)
	}
	u := srv.URL
	srv.Close()
	if _, err := probeURL(u); err == nil {
		t.Fatal("closed server: want error")
	}
}
