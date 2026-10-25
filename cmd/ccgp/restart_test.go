package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/angel"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server"
)

func TestServerStale(t *testing.T) {
	v := server.ProxyVersion
	tests := []struct {
		name      string
		h         *angel.Health
		ownExe    string
		ownExeMod int64
		wantStale bool
		wantWhy   string
	}{
		{name: "same version and exe", h: &angel.Health{Version: v, Exe: "/ccgp", ExeMod: 10}, ownExe: "/ccgp", ownExeMod: 10},
		{name: "version differs", h: &angel.Health{Version: "0.0.9"}, wantStale: true, wantWhy: "server version 0.0.9, this binary is " + v},
		{name: "exe file replaced", h: &angel.Health{Version: v, Exe: "/ccgp", ExeMod: 10}, ownExe: "/ccgp", ownExeMod: 20, wantStale: true, wantWhy: "the executable file changed since the server started"},
		{name: "different exe path", h: &angel.Health{Version: v, Exe: "/old/ccgp", ExeMod: 10}, ownExe: "/new/ccgp", ownExeMod: 20},
		{name: "server exe unknown", h: &angel.Health{Version: v}, ownExe: "/ccgp", ownExeMod: 20},
		{name: "server mod time unknown", h: &angel.Health{Version: v, Exe: "/ccgp"}, ownExe: "/ccgp", ownExeMod: 20},
		{name: "own mod time unknown", h: &angel.Health{Version: v, Exe: "/ccgp", ExeMod: 10}, ownExe: "/ccgp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stale, why := serverStale(tt.h, v, tt.ownExe, tt.ownExeMod)
			if stale != tt.wantStale || why != tt.wantWhy {
				t.Errorf("serverStale = (%v, %q), want (%v, %q)", stale, why, tt.wantStale, tt.wantWhy)
			}
		})
	}
}

type fakeChecks struct {
	serverChecks
	checkHealth *angel.Health
	checkErr    error
	stopped     int
	started     int
	startHealth *angel.Health
}

func newFakeChecks(h *angel.Health) *fakeChecks {
	fc := &fakeChecks{checkHealth: h}
	fc.check = func(string) (*angel.Health, error) {
		if fc.checkErr != nil {
			return nil, fc.checkErr
		}
		return fc.checkHealth, nil
	}
	fc.stop = func(string, time.Duration) error { fc.stopped++; return nil }
	fc.start = func(string, []string, string, string, time.Duration) (*angel.Health, error) {
		fc.started++
		return fc.startHealth, nil
	}
	fc.stderr = &bytes.Buffer{}
	fc.ownVersion = server.ProxyVersion
	return fc
}

func TestEnsureServerStaleIdleRestarts(t *testing.T) {
	fc := newFakeChecks(&angel.Health{Version: "old", PID: 1, Active: 0})
	fc.startHealth = &angel.Health{Version: server.ProxyVersion, PID: 2}
	h, err := ensureServerWith(&config.Config{Host: "127.0.0.1", Port: "1"}, nil, &fc.serverChecks)
	if err != nil {
		t.Fatal(err)
	}
	if h.PID != 2 {
		t.Errorf("pid = %d, want 2 (restarted)", h.PID)
	}
	if fc.stopped != 1 || fc.started != 1 {
		t.Errorf("stopped=%d started=%d, want 1/1", fc.stopped, fc.started)
	}
	if out := fc.stderr.(*bytes.Buffer).String(); !strings.Contains(out, "ccgp: restarted the server (") {
		t.Errorf("stderr = %q, want success line after restart", out)
	}
}

func TestEnsureServerStaleStartFailsNoSuccessLine(t *testing.T) {
	fc := newFakeChecks(&angel.Health{Version: "old"})
	fc.start = func(string, []string, string, string, time.Duration) (*angel.Health, error) {
		return nil, errors.New("start boom")
	}
	if _, err := ensureServerWith(&config.Config{Host: "127.0.0.1", Port: "1"}, nil, &fc.serverChecks); err == nil {
		t.Fatal("want start error")
	}
	if out := fc.stderr.(*bytes.Buffer).String(); strings.Contains(out, "restarted the server") {
		t.Errorf("stderr = %q, want no success line on start failure", out)
	}
}

func TestEnsureServerStaleBusyWarnsAndKeeps(t *testing.T) {
	fc := newFakeChecks(&angel.Health{Version: "old", PID: 1, Active: 3})
	h, err := ensureServerWith(&config.Config{Host: "127.0.0.1", Port: "1"}, nil, &fc.serverChecks)
	if err != nil {
		t.Fatal(err)
	}
	if h.PID != 1 {
		t.Errorf("pid = %d, want 1 (kept)", h.PID)
	}
	if fc.stopped != 0 || fc.started != 0 {
		t.Errorf("stopped=%d started=%d, want 0/0", fc.stopped, fc.started)
	}
	out := fc.stderr.(*bytes.Buffer).String()
	if !strings.Contains(out, "older than this binary") || !strings.Contains(out, "3 request(s) in flight") {
		t.Errorf("stderr = %q", out)
	}
}

func TestEnsureServerFreshUntouched(t *testing.T) {
	fc := newFakeChecks(&angel.Health{Version: server.ProxyVersion, PID: 7})
	h, err := ensureServerWith(&config.Config{Host: "127.0.0.1", Port: "1"}, nil, &fc.serverChecks)
	if err != nil {
		t.Fatal(err)
	}
	if h.PID != 7 {
		t.Errorf("pid = %d, want 7", h.PID)
	}
	if fc.stopped != 0 || fc.started != 0 {
		t.Errorf("stopped=%d started=%d, want 0/0", fc.stopped, fc.started)
	}
	if out := fc.stderr.(*bytes.Buffer).String(); out != "" {
		t.Errorf("stderr = %q, want empty", out)
	}
}

func TestEnsureServerStaleStopFails(t *testing.T) {
	fc := newFakeChecks(&angel.Health{Version: "old"})
	fc.stop = func(string, time.Duration) error { return errors.New("boom") }
	if _, err := ensureServerWith(&config.Config{Host: "127.0.0.1", Port: "1"}, nil, &fc.serverChecks); err == nil || err.Error() != "boom" {
		t.Errorf("err = %v, want boom", err)
	}
	if out := fc.stderr.(*bytes.Buffer).String(); strings.Contains(out, "restarted the server") {
		t.Errorf("stderr = %q, want no success line on stop failure", out)
	}
}

type fakeOps struct {
	restartOps
	running *angel.Health
	stopped int
	started int
}

func newFakeOps(running *angel.Health) *fakeOps {
	fo := &fakeOps{running: running}
	fo.stop = func(string, time.Duration) error {
		if fo.running == nil {
			return angel.ErrNotRunning
		}
		fo.stopped++
		return nil
	}
	fo.start = func(string, []string, string, string, time.Duration) (*angel.Health, error) {
		fo.started++
		return &angel.Health{Version: server.ProxyVersion, PID: 100 + fo.started}, nil
	}
	fo.stdout = &bytes.Buffer{}
	fo.exe = func() (string, error) { return "/ccgp", nil }
	fo.logPath = func() (string, error) { return "/log", nil }
	return fo
}

var restartCfg = &config.Config{Host: "127.0.0.1", Port: "1"}

func TestRestartServerNotRunningJustStarts(t *testing.T) {
	fo := newFakeOps(nil)
	h, err := restartServer(restartCfg, &fo.restartOps)
	if err != nil {
		t.Fatal(err)
	}
	if fo.stopped != 0 || fo.started != 1 {
		t.Errorf("stopped=%d started=%d, want 0/1", fo.stopped, fo.started)
	}
	if h.PID != 101 {
		t.Errorf("pid = %d, want 101", h.PID)
	}
	if out := fo.stdout.(*bytes.Buffer).String(); out != "ccgp restarted (pid 101) at http://127.0.0.1:1\n" {
		t.Errorf("stdout = %q", out)
	}
}

func TestRestartServerRunningStopsThenStarts(t *testing.T) {
	fo := newFakeOps(&angel.Health{PID: 5, Active: 4})
	h, err := restartServer(restartCfg, &fo.restartOps)
	if err != nil {
		t.Fatal(err)
	}
	if fo.stopped != 1 || fo.started != 1 {
		t.Errorf("stopped=%d started=%d, want 1/1", fo.stopped, fo.started)
	}
	if h.PID != 101 {
		t.Errorf("pid = %d, want 101 (new server despite 4 in flight)", h.PID)
	}
}

func TestRestartServerStopErrorPropagates(t *testing.T) {
	fo := newFakeOps(&angel.Health{PID: 5})
	fo.stop = func(string, time.Duration) error { return errors.New("stop boom") }
	if _, err := restartServer(restartCfg, &fo.restartOps); err == nil || err.Error() != "stop boom" {
		t.Errorf("err = %v, want stop boom", err)
	}
	if fo.started != 0 {
		t.Errorf("started = %d, want 0", fo.started)
	}
	if out := fo.stdout.(*bytes.Buffer).String(); out != "" {
		t.Errorf("stdout = %q, want empty on failure", out)
	}
}

func TestRestartServerStartErrorPropagates(t *testing.T) {
	fo := newFakeOps(nil)
	fo.start = func(string, []string, string, string, time.Duration) (*angel.Health, error) {
		return nil, errors.New("start boom")
	}
	if _, err := restartServer(restartCfg, &fo.restartOps); err == nil || err.Error() != "start boom" {
		t.Errorf("err = %v, want start boom", err)
	}
	if out := fo.stdout.(*bytes.Buffer).String(); out != "" {
		t.Errorf("stdout = %q, want empty on failure", out)
	}
}

func TestOwnExeInfo(t *testing.T) {
	exe, mod := ownExeInfo()
	if exe == "" {
		t.Fatal("exe path is empty")
	}
	if mod == 0 {
		t.Error("mod time of the test binary should be known")
	}
}

// A plain first start (nothing running) is not a restart: no message.
func TestEnsureServerNotRunningStartsSilently(t *testing.T) {
	fc := newFakeChecks(nil)
	fc.checkErr = errors.New("connection refused")
	fc.startHealth = &angel.Health{Version: server.ProxyVersion, PID: 5}
	h, err := ensureServerWith(&config.Config{Host: "127.0.0.1", Port: "1"}, nil, &fc.serverChecks)
	if err != nil || h.PID != 5 {
		t.Fatalf("h=%v err=%v", h, err)
	}
	if fc.stopped != 0 || fc.started != 1 {
		t.Errorf("stopped=%d started=%d, want 0/1", fc.stopped, fc.started)
	}
	if out := fc.stderr.(*bytes.Buffer).String(); out != "" {
		t.Errorf("stderr = %q, want empty", out)
	}
}
