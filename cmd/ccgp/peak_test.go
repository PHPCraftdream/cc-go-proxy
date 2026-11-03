package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

const peakTestBody = "listen.port: 60093\n\nproviders: [\n    {\n        name: p\n        type: codex\n    }\n    {\n        name: work\n        type: claude\n    }\n]\n\nprofiles: [\n    {\n        name: pp\n        provider: p\n    }\n    {\n        name: work\n        provider: work\n    }\n]\n"

func peakTestConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(p, []byte(peakTestBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, p)
	return p
}

// 2026-11-03 is a Tuesday.
var peakNow = time.Date(2026, 11, 3, 15, 42, 0, 0, time.FixedZone("+08:00", 8*3600))

func runPeak(t *testing.T, pauses *config.PeakPauses, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := peak(&buf, peakNow, pauses, args)
	return buf.String(), err
}

func TestPeakShowsProvidersWithoutPeakHours(t *testing.T) {
	peakTestConfig(t)
	out, err := runPeak(t, config.NewPeakPauses(t.TempDir()))
	if err != nil || out != "p: no peak hours\n" {
		t.Errorf("output %q, %v: want p only (a Claude provider has none)", out, err)
	}
}

func TestPeakSetAddRemoveClear(t *testing.T) {
	path := peakTestConfig(t)
	pauses := config.NewPeakPauses(t.TempDir())
	file := func() string {
		b, _ := os.ReadFile(path)
		return string(b)
	}

	out, err := runPeak(t, pauses, "p", "set", "mon-fri", "14:00-18:00")
	if err != nil || !strings.Contains(out, "config.ktav updated") || !strings.Contains(out, "p: peak hours in local time") || !strings.Contains(out, "  1  mon-fri 14:00-18:00\n") {
		t.Fatalf("set: %v\n%s", err, out)
	}
	if !strings.Contains(file(), "        type: codex\n        peak: [\n            {\n                days: mon-fri\n                from: 14:00\n                to: 18:00\n            }\n        ]\n") {
		t.Errorf("config after set:\n%s", file())
	}

	// A bare +8 is kept as +08:00: a number would not read back as a string.
	out, err = runPeak(t, pauses, "p", "tz", "+8")
	if err != nil || !strings.Contains(out, "peak hours in +08:00 (there it is Tue 15:42 now)") ||
		!strings.Contains(out, "now: PEAK, requests are refused until 18:00 +08:00 (") {
		t.Fatalf("tz: %v\n%s", err, out)
	}
	if !strings.Contains(file(), "        type: codex\n        peak_tz: +08:00\n        peak: [") {
		t.Errorf("config after tz:\n%s", file())
	}

	// add takes several windows and ignores one that is there already.
	out, err = runPeak(t, pauses, "pp", "add", "sat,sun", "10:00-12:00", "mon-fri", "14:00-18:00")
	if err != nil || !strings.Contains(out, "  1  mon-fri 14:00-18:00\n  2  sat,sun 10:00-12:00\n") || strings.Contains(out, "  3  ") {
		t.Fatalf("add (by a profile name): %v\n%s", err, out)
	}

	if _, err := runPeak(t, pauses, "p", "rm", "5"); err == nil {
		t.Error("rm of a window that is not there succeeded")
	}
	if _, err := runPeak(t, pauses, "p", "rm"); err == nil {
		t.Error("rm without a number succeeded")
	}
	out, err = runPeak(t, pauses, "p", "rm", "1")
	if err != nil || strings.Contains(out, "mon-fri") || !strings.Contains(out, "  1  sat,sun 10:00-12:00\n") || !strings.Contains(out, "now: open, the next peak starts Sat 10:00 +08:00 (") {
		t.Fatalf("rm: %v\n%s", err, out)
	}

	// Removing the last window drops the zone with it; so does clear.
	if _, err := runPeak(t, pauses, "p", "rm", "1"); err != nil || file() != peakTestBody {
		t.Errorf("rm of the last window: %v\n%s", err, file())
	}
	if _, err := runPeak(t, pauses, "p", "set", "20:00-23:00"); err != nil {
		t.Fatal(err)
	}
	out, err = runPeak(t, pauses, "p", "clear")
	if err != nil || file() != peakTestBody || !strings.Contains(out, "p: no peak hours") {
		t.Errorf("clear: %v\n%s\n%s", err, out, file())
	}
	if out, err := runPeak(t, pauses, "p", "clear"); err != nil || !strings.Contains(out, "already had these peak hours") {
		t.Errorf("clear of nothing: %v\n%s", err, out)
	}
}

func TestPeakRejectsBadInput(t *testing.T) {
	path := peakTestConfig(t)
	pauses := config.NewPeakPauses(t.TempDir())
	for _, args := range [][]string{
		{"nope"},
		{"work"},
		{"p", "set"},
		{"p", "set", "mon-fri"},
		{"p", "set", "14:00-18:00", "extra"},
		{"p", "set", "someday", "14:00-18:00"},
		{"p", "add", "25:00-26:00"},
		{"p", "tz", "UTC"},
		{"p", "bogus"},
		{"p", "clear", "x"},
		{"p", "pause"},
		{"p", "pause", "2h"},
		{"p", "resume", "x"},
	} {
		if out, err := runPeak(t, pauses, args...); err == nil {
			t.Errorf("%q: no error, output %q", args, out)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != peakTestBody {
		t.Errorf("a refused command changed the config:\n%s", b)
	}
	if _, err := runPeak(t, pauses, "p", "set", "14:00-18:00"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"p", "tz", "Mars/Base"}, {"p", "tz"}, {"p", "pause", "soon"}, {"p", "pause", "-1h"}, {"p", "pause", "8d"}, {"p", "pause", "1h", "2h"}} {
		if out, err := runPeak(t, pauses, args...); err == nil {
			t.Errorf("%q: no error, output %q", args, out)
		}
	}
}

func TestPeakPauseAndResume(t *testing.T) {
	peakTestConfig(t)
	pauses := config.NewPeakPauses(t.TempDir())
	if _, err := runPeak(t, pauses, "p", "set", "mon-fri", "14:00-18:00"); err != nil {
		t.Fatal(err)
	}
	if _, err := runPeak(t, pauses, "p", "tz", "+08:00"); err != nil {
		t.Fatal(err)
	}

	// Without a length the pause lasts to the end of the running peak.
	out, err := runPeak(t, pauses, "p", "pause")
	if err != nil || !strings.Contains(out, "peak hours of p do not refuse requests until 18:00 +08:00 (") {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if until, ok := pauses.Until("p", peakNow); !ok || !until.Equal(peakNow.Add(2*time.Hour+18*time.Minute)) {
		t.Errorf("pause until %v, %v", until, ok)
	}
	out, _ = runPeak(t, pauses, "p")
	if !strings.Contains(out, "now: peak, but paused: requests go through until 18:00 +08:00") {
		t.Errorf("show during a pause:\n%s", out)
	}

	out, err = runPeak(t, pauses, "p", "resume")
	if err != nil || !strings.Contains(out, "peak hours of p apply again") {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if _, ok := pauses.Until("p", peakNow); ok {
		t.Error("still paused after resume")
	}

	// A length works outside peak hours too.
	if _, err := runPeak(t, pauses, "p", "pause", "1d"); err != nil {
		t.Fatal(err)
	}
	if until, ok := pauses.Until("p", peakNow); !ok || !until.Equal(peakNow.Add(24*time.Hour)) {
		t.Errorf("pause 1d until %v, %v", until, ok)
	}
}

func TestPeakJSON(t *testing.T) {
	peakTestConfig(t)
	pauses := config.NewPeakPauses(t.TempDir())
	for _, args := range [][]string{{"p", "set", "mon-fri", "14:00-18:00"}, {"p", "tz", "+08:00"}} {
		if _, err := runPeak(t, pauses, args...); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runPeak(t, pauses, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]struct {
		Windows []config.PeakWindow
		Zone    string
		PeakNow bool   `json:"peak_now"`
		Until   string `json:"until"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	p := got["p"]
	if len(got) != 1 || len(p.Windows) != 1 || p.Windows[0].Days != "mon-fri" || p.Zone != "+08:00" || !p.PeakNow || p.Until != "2026-11-03T18:00:00+08:00" {
		t.Errorf("json:\n%s", out)
	}
}

func TestParsePause(t *testing.T) {
	for in, want := range map[string]time.Duration{"90m": 90 * time.Minute, "2h": 2 * time.Hour, "1h30m": 90 * time.Minute, "1d": 24 * time.Hour, "7d": 7 * 24 * time.Hour} {
		if got, err := parsePause(in); err != nil || got != want {
			t.Errorf("parsePause(%q) = %v, %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "0d", "-5m", "x", "d", "8d", "200h"} {
		if got, err := parsePause(in); err == nil {
			t.Errorf("parsePause(%q) = %v, want an error", in, got)
		}
	}
}

// A window over all time has no end to name, and a pause needs a length then.
func TestPeakAroundTheClock(t *testing.T) {
	peakTestConfig(t)
	pauses := config.NewPeakPauses(t.TempDir())
	out, err := runPeak(t, pauses, "p", "set", "00:00-24:00")
	if err != nil || !strings.Contains(out, "now: PEAK, requests are refused (the windows cover all time)") {
		t.Fatalf("set: %v\n%s", err, out)
	}
	if _, err := runPeak(t, pauses, "p", "pause"); err == nil {
		t.Error("a pause to the end of a peak that has none")
	}
	out, err = runPeak(t, pauses, "p", "pause", "1h")
	if err != nil || !strings.Contains(out, "do not refuse requests until") {
		t.Fatalf("pause 1h: %v\n%s", err, out)
	}
	out, _ = runPeak(t, pauses, "p")
	if !strings.Contains(out, "now: peak, but paused: requests go through until") {
		t.Errorf("show:\n%s", out)
	}
}

// Without a config file there is nothing to edit.
func TestChangePeakNeedsAConfigFile(t *testing.T) {
	err := changePeak(&bytes.Buffer{}, &config.Config{}, peakNow, config.NewPeakPauses(t.TempDir()), "p",
		[]config.PeakWindow{{From: "14:00", To: "18:00"}}, "")
	if err == nil || !strings.Contains(err.Error(), "env-only mode") {
		t.Errorf("changePeak = %v, want the env-only refusal", err)
	}
}
