package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParsePeakWords(t *testing.T) {
	got, err := ParsePeakWords([]string{"Mon-Fri", "14:00-18:00", "9:30-10:00", "sat,sun 10:00-12:00"})
	want := []PeakWindow{
		{Days: "mon-fri", From: "14:00", To: "18:00"},
		{From: "09:30", To: "10:00"},
		{Days: "sat,sun", From: "10:00", To: "12:00"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ParsePeakWords = %+v, %v\nwant %+v", got, err, want)
	}
	for _, bad := range [][]string{
		nil,
		{"mon-fri"},
		{"mon", "tue", "14:00-18:00"},
		{"14:00-14:00"},
		{"25:00-26:00"},
		{"24:00-02:00"},
		{"someday", "14:00-18:00"},
		{"mon-fri", "14:00-18:00", "tue"},
		{"14:00-18:60"},
	} {
		if got, err := ParsePeakWords(bad); err == nil {
			t.Errorf("ParsePeakWords(%q) = %+v, want an error", bad, got)
		}
	}
}

func TestPeakWindowString(t *testing.T) {
	if s := (PeakWindow{Days: "mon-fri", From: "14:00", To: "18:00"}).String(); s != "mon-fri 14:00-18:00" {
		t.Errorf("String = %q", s)
	}
	if s := (PeakWindow{From: "20:00", To: "23:00"}).String(); s != "20:00-23:00" {
		t.Errorf("String = %q", s)
	}
}

func TestParseDays(t *testing.T) {
	for in, want := range map[string]uint8{
		"":            0x7f,
		"mon":         1 << 1,
		"sat,sun":     1<<6 | 1,
		"mon-fri":     1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5,
		"fri-mon":     1<<5 | 1<<6 | 1 | 1<<1,
		"SUN":         1,
		"mon-wed,fri": 1<<1 | 1<<2 | 1<<3 | 1<<5,
	} {
		if got, err := parseDays(in); err != nil || got != want {
			t.Errorf("parseDays(%q) = %#x, %v, want %#x", in, got, err, want)
		}
	}
	for _, bad := range []string{"mo", "mon-", "-fri", "mon,,tue", "monday"} {
		if _, err := parseDays(bad); err == nil {
			t.Errorf("parseDays(%q) accepted", bad)
		}
	}
}

func TestParsePeakZone(t *testing.T) {
	jan := func(loc *time.Location) int {
		_, off := time.Date(2026, 1, 1, 0, 0, 0, 0, loc).Zone()
		return off
	}
	for in, want := range map[string]int{
		"+08:00": 8 * 3600, "+8": 8 * 3600, "+0800": 8 * 3600, "UTC+8": 8 * 3600,
		"-05:30": -(5*3600 + 1800), "GMT-3": -3 * 3600, "UTC": 0, "Asia/Shanghai": 8 * 3600,
	} {
		loc, err := ParsePeakZone(in)
		if err != nil || jan(loc) != want {
			t.Errorf("ParsePeakZone(%q) = %v (%d), %v, want offset %d", in, loc, jan(loc), err, want)
		}
	}
	if loc, err := ParsePeakZone(""); err != nil || loc != time.Local {
		t.Errorf("empty zone = %v, %v, want the machine's", loc, err)
	}
	for _, bad := range []string{"Mars/Base", "+15:00", "+08:75", "+1:2:3", "nonsense"} {
		if _, err := ParsePeakZone(bad); err == nil {
			t.Errorf("ParsePeakZone(%q) accepted", bad)
		}
	}
	for in, want := range map[string]string{"+8": "+08:00", "UTC-3": "-03:00", "gmt": "UTC", "Asia/Shanghai": "Asia/Shanghai", "-0530": "-05:30"} {
		if got, err := CanonicalPeakZone(in); err != nil || got != want {
			t.Errorf("CanonicalPeakZone(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
}

// 2026-11-03 is a Tuesday.
func at(zone string, day, hour, min int) time.Time {
	loc, _ := ParsePeakZone(zone)
	return time.Date(2026, 11, day, hour, min, 0, 0, loc)
}

func mustSchedule(t *testing.T, tz string, windows ...PeakWindow) *PeakSchedule {
	t.Helper()
	s, err := NewPeakSchedule(windows, tz)
	if err != nil || s == nil {
		t.Fatalf("NewPeakSchedule: %v", err)
	}
	return s
}

func TestPeakAtWeekdayWindow(t *testing.T) {
	s := mustSchedule(t, "+08:00", PeakWindow{Days: "mon-fri", From: "14:00", To: "18:00"})
	tests := []struct {
		name    string
		now     time.Time
		in      bool
		changes time.Time
	}{
		{"inside", at("+08:00", 3, 15, 42), true, at("+08:00", 3, 18, 0)},
		{"first second", at("+08:00", 3, 14, 0), true, at("+08:00", 3, 18, 0)},
		{"just before", at("+08:00", 3, 13, 59), false, at("+08:00", 3, 14, 0)},
		{"end is exclusive", at("+08:00", 3, 18, 0), false, at("+08:00", 4, 14, 0)},
		{"friday evening", at("+08:00", 6, 19, 0), false, at("+08:00", 9, 14, 0)},
		{"saturday", at("+08:00", 7, 15, 0), false, at("+08:00", 9, 14, 0)},
		{"given in another zone", at("UTC", 3, 8, 0), true, at("+08:00", 3, 18, 0)},
	}
	for _, tt := range tests {
		st := s.At(tt.now)
		if st.In != tt.in || !st.Until.Equal(tt.changes) {
			t.Errorf("%s: At = in %v until %v, want in %v until %v", tt.name, st.In, st.Until, tt.in, tt.changes)
		}
	}
	if st := s.At(at("+08:00", 3, 15, 0)); len(st.Windows) != 1 || st.Windows[0].Days != "mon-fri" {
		t.Errorf("windows = %+v", st.Windows)
	}
}

func TestPeakAtPastMidnight(t *testing.T) {
	s := mustSchedule(t, "+08:00", PeakWindow{Days: "fri", From: "22:00", To: "06:00"})
	if st := s.At(at("+08:00", 6, 23, 0)); !st.In || !st.Until.Equal(at("+08:00", 7, 6, 0)) {
		t.Errorf("friday 23:00: %+v", st)
	}
	if st := s.At(at("+08:00", 7, 5, 59)); !st.In {
		t.Errorf("saturday 05:59 is still the friday window: %+v", st)
	}
	if st := s.At(at("+08:00", 7, 6, 0)); st.In {
		t.Errorf("saturday 06:00 is out: %+v", st)
	}
	if st := s.At(at("+08:00", 8, 3, 0)); st.In {
		t.Errorf("sunday 03:00 follows no friday window: %+v", st)
	}
	if st := s.At(at("+08:00", 3, 23, 0)); st.In || !st.Until.Equal(at("+08:00", 6, 22, 0)) {
		t.Errorf("tuesday 23:00: %+v, want the next start on friday 22:00", st)
	}
}

func TestPeakAtTouchingWindowsAreOneRun(t *testing.T) {
	s := mustSchedule(t, "UTC",
		PeakWindow{From: "14:00", To: "18:00"}, PeakWindow{From: "18:00", To: "20:00"})
	if st := s.At(at("UTC", 3, 15, 0)); !st.In || !st.Until.Equal(at("UTC", 3, 20, 0)) {
		t.Errorf("At = %+v, want in until 20:00", st)
	}
}

func TestPeakAtAllTheTime(t *testing.T) {
	s := mustSchedule(t, "UTC", PeakWindow{From: "00:00", To: "24:00"})
	if st := s.At(at("UTC", 3, 15, 0)); !st.In || !st.Until.IsZero() {
		t.Errorf("a window over all time never ends: %+v", st)
	}
}

func TestPeakAtInANamedZone(t *testing.T) {
	// New York leaves daylight time on 2026-11-01: 09:00 is UTC-5 afterwards.
	s := mustSchedule(t, "America/New_York", PeakWindow{From: "09:00", To: "17:00"})
	ny, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 11, 3, 14, 30, 0, 0, time.UTC) // 09:30 in New York
	if st := s.At(now); !st.In || !st.Until.Equal(time.Date(2026, 11, 3, 17, 0, 0, 0, ny)) {
		t.Errorf("At = %+v", st)
	}
}

func TestNewPeakSchedule(t *testing.T) {
	if s, err := NewPeakSchedule(nil, ""); s != nil || err != nil {
		t.Errorf("no windows = %v, %v, want nil", s, err)
	}
	if _, err := NewPeakSchedule(nil, "+08:00"); err == nil {
		t.Error("a zone without windows was accepted")
	}
	if _, err := NewPeakSchedule([]PeakWindow{{From: "14:00", To: "18:00"}}, "Mars/Base"); err == nil {
		t.Error("a bad zone was accepted")
	}
	if _, err := NewPeakSchedule([]PeakWindow{{From: "14:00", To: "18:00"}, {Days: "xx", From: "1:00", To: "2:00"}}, ""); err == nil || !strings.Contains(err.Error(), "peak[1]") {
		t.Errorf("a bad second window: %v, want it named", err)
	}
}

const peakBase = `## config
providers: [
    {
        name: zai
        type: anthropic_compat
        base_url: https://api.z.ai/api/anthropic
        effort_map: {
            low: low
        }
    }
    {
        name: codex
        type: codex
    }
]
profiles: [
    {
        name: zai
        provider: zai
    }
    {
        name: codex
        provider: codex
    }
]
`

func TestLoadPeakHours(t *testing.T) {
	text := strings.Replace(peakBase, "        base_url: https://api.z.ai/api/anthropic\n", `        base_url: https://api.z.ai/api/anthropic
        peak_tz: Asia/Shanghai
        peak: [
            {
                days: mon-fri
                from: 14:00
                to: 18:00
            }
            {
                from: 20:00
                to: 24:00
            }
        ]
`, 1)
	fc := mustLoad(t, text)
	zai := providerPtr(fc, "zai")
	want := []PeakWindow{{Days: "mon-fri", From: "14:00", To: "18:00"}, {From: "20:00", To: "24:00"}}
	if zai.PeakTZ != "Asia/Shanghai" || !reflect.DeepEqual(zai.Peak, want) {
		t.Errorf("zai peak = %+v in %q", zai.Peak, zai.PeakTZ)
	}
	if s, err := zai.PeakSchedule(); err != nil || s == nil {
		t.Errorf("PeakSchedule = %v, %v", s, err)
	}
	if codex := providerPtr(fc, "codex"); len(codex.Peak) != 0 {
		t.Errorf("codex peak = %+v", codex.Peak)
	}
}

func TestLoadRejectsBadPeakHours(t *testing.T) {
	for name, extra := range map[string]string{
		"bad time":     "        peak: [\n            {\n                from: 25:00\n                to: 26:00\n            }\n        ]\n",
		"bad day":      "        peak: [\n            {\n                days: funday\n                from: 14:00\n                to: 18:00\n            }\n        ]\n",
		"empty window": "        peak: [\n            {\n                from: 14:00\n                to: 14:00\n            }\n        ]\n",
		"zone alone":   "        peak_tz: Asia/Shanghai\n",
		"bad zone":     "        peak_tz: Mars/Base\n        peak: [\n            {\n                from: 14:00\n                to: 18:00\n            }\n        ]\n",
	} {
		text := strings.Replace(peakBase, "        base_url: https://api.z.ai/api/anthropic\n", "        base_url: https://api.z.ai/api/anthropic\n"+extra, 1)
		if _, err := loadFile(writeKtav(t, text)); err == nil || !strings.Contains(err.Error(), `provider "zai"`) {
			t.Errorf("%s: loadFile = %v, want an error naming the provider", name, err)
		}
	}
	claude := "providers: [\n    {\n        name: work\n        type: claude\n        peak: [\n            {\n                from: 14:00\n                to: 18:00\n            }\n        ]\n    }\n]\nprofiles: [\n    {\n        name: work\n        provider: work\n    }\n]\n"
	if _, err := loadFile(writeKtav(t, claude)); err == nil {
		t.Error("a claude provider with peak hours was accepted")
	}
}

func TestSetProviderPeakText(t *testing.T) {
	windows := []PeakWindow{{Days: "mon-fri", From: "14:00", To: "18:00"}, {From: "20:00", To: "23:00"}}
	got, err := SetProviderPeakText(peakBase, "zai", windows, "Asia/Shanghai")
	want := strings.Replace(peakBase, "        base_url: https://api.z.ai/api/anthropic\n", `        base_url: https://api.z.ai/api/anthropic
        peak_tz: Asia/Shanghai
        peak: [
            {
                days: mon-fri
                from: 14:00
                to: 18:00
            }
            {
                from: 20:00
                to: 23:00
            }
        ]
`, 1)
	if err != nil || got != want {
		t.Fatalf("set: %v\n%s\nwant\n%s", err, got, want)
	}
	fc := mustLoad(t, got)
	if zai := providerPtr(fc, "zai"); !reflect.DeepEqual(zai.Peak, windows) || zai.PeakTZ != "Asia/Shanghai" {
		t.Errorf("loaded back: %+v in %q", zai.Peak, zai.PeakTZ)
	}

	// A new list replaces the old one, and a missing zone removes the old zone.
	one := []PeakWindow{{From: "01:00", To: "02:00"}}
	again, err := SetProviderPeakText(got, "zai", one, "")
	if err != nil || strings.Contains(again, "peak_tz") || strings.Contains(again, "14:00") || !strings.Contains(again, "from: 01:00") {
		t.Fatalf("replace: %v\n%s", err, again)
	}
	mustLoad(t, again)

	// Clearing gives the original text back; clearing nothing changes nothing.
	if back, err := SetProviderPeakText(got, "zai", nil, ""); err != nil || back != peakBase {
		t.Errorf("clear: %v\n%s", err, back)
	}
	if same, err := SetProviderPeakText(peakBase, "zai", nil, ""); err != nil || same != peakBase {
		t.Errorf("clear of nothing: %v\n%s", err, same)
	}

	// Another provider stays as it was.
	if !strings.Contains(got, "    {\n        name: codex\n        type: codex\n    }\n") {
		t.Errorf("codex was touched:\n%s", got)
	}
	if _, err := SetProviderPeakText(peakBase, "nope", windows, ""); err == nil {
		t.Error("unknown provider accepted")
	}
	if _, err := SetProviderPeakText(peakBase, "zai", []PeakWindow{{From: "9", To: "10"}}, ""); err == nil {
		t.Error("bad window accepted")
	}

	// CRLF files keep their endings.
	crlf := strings.ReplaceAll(peakBase, "\n", "\r\n")
	got3, err := SetProviderPeakText(crlf, "codex", one, "+08:00")
	if err != nil || strings.Contains(strings.ReplaceAll(got3, "\r\n", ""), "\n") || !strings.Contains(got3, "        type: codex\r\n        peak_tz: +08:00\r\n        peak: [\r\n") {
		t.Errorf("CRLF: %v\n%q", err, got3)
	}
	mustLoad(t, got3)
}

func TestPeakPauses(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)
	p := NewPeakPauses(dir)
	if _, ok := p.Until("zai", now); ok {
		t.Fatal("a pause from nowhere")
	}
	until := now.Add(2 * time.Hour)
	if err := p.Set("zai", until, now); err != nil {
		t.Fatal(err)
	}
	if got, ok := p.Until("zai", now); !ok || !got.Equal(until) {
		t.Errorf("Until = %v, %v, want %v", got, ok, until)
	}
	if _, ok := p.Until("zai", until); ok {
		t.Error("the pause outlived its end")
	}
	if _, ok := p.Until("codex", now); ok {
		t.Error("another provider is paused")
	}

	// A second reader (the server) sees what the first one (the CLI) wrote, and a change.
	srv := NewPeakPauses(dir)
	if got, ok := srv.Until("zai", now); !ok || !got.Equal(until) {
		t.Errorf("server Until = %v, %v", got, ok)
	}
	later := now.Add(5 * time.Hour)
	if err := p.Set("zai", later, now); err != nil {
		t.Fatal(err)
	}
	if got, ok := srv.Until("zai", now); !ok || !got.Equal(later) {
		t.Errorf("server did not see the change: %v, %v", got, ok)
	}

	// A rewrite drops the pauses that have run out.
	if err := p.Set("codex", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := p.Set("kimi", now.Add(4*time.Hour), now.Add(2*time.Hour)); err != nil {
		t.Fatal(err) // at 17:00 the codex pause (until 16:00) has run out
	}
	if _, ok := p.Until("codex", now); ok {
		t.Error("a pause that ran out was kept")
	}
	if _, ok := p.Until("zai", now); !ok {
		t.Error("a running pause was dropped")
	}

	// Ending the last running pause removes the file.
	for _, name := range []string{"zai", "kimi"} {
		if err := p.Set(name, time.Time{}, now.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, pauseFile)); err == nil {
		t.Error("the file stays with no pause in it")
	}

	if err := NewPeakPauses("").Set("zai", until, now); err == nil {
		t.Error("a pause without a directory was written")
	}
	if _, ok := NewPeakPauses("").Until("zai", now); ok {
		t.Error("a pause without a directory")
	}
}

func TestFormatWait(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                            "1m",
		10 * time.Second:             "1m",
		time.Minute:                  "1m",
		61 * time.Second:             "2m",
		45 * time.Minute:             "45m",
		time.Hour:                    "1h",
		2*time.Hour + 18*time.Minute: "2h 18m",
		2*time.Hour + 17*time.Minute + 1*time.Second: "2h 18m", // rounded up: never promise less
		24 * time.Hour:                "1d",
		28 * time.Hour:                "1d 4h",
		49*time.Hour + 30*time.Minute: "2d 1h",
	} {
		if got := FormatWait(d); got != want {
			t.Errorf("FormatWait(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestClockText(t *testing.T) {
	now := time.Date(2026, 11, 3, 15, 0, 0, 0, time.Local) // a Tuesday

	// The machine's own zone: one clock, no zone name.
	local := mustSchedule(t, "", PeakWindow{From: "14:00", To: "18:00"})
	if got := local.ClockText(now.Add(3*time.Hour), now); got != "18:00 (in 3h)" {
		t.Errorf("same day: %q", got)
	}
	if got := local.ClockText(now.Add(24*time.Hour), now); got != "Wed 15:00 (in 1d)" {
		t.Errorf("next day: %q", got)
	}

	// Another zone: its clock and name first, then the machine's clock.
	shanghai := mustSchedule(t, "+08:00", PeakWindow{From: "14:00", To: "18:00"})
	utc := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC) // 23:00 at +08:00
	got := shanghai.ClockText(utc.Add(2*time.Hour), utc)
	if !strings.HasPrefix(got, "Wed 01:00 +08:00 (") || !strings.HasSuffix(got, " your time, in 2h)") {
		t.Errorf("other zone: %q", got)
	}
}

func TestPeakAtFollowsDaylightSaving(t *testing.T) {
	// New York moves to daylight time on 2026-03-08: 09:00 is UTC-5 before it, UTC-4 after.
	s := mustSchedule(t, "America/New_York", PeakWindow{From: "09:00", To: "17:00"})
	for _, tt := range []struct {
		name string
		utc  time.Time
		in   bool
	}{
		{"08:30 EST", time.Date(2026, 3, 7, 13, 30, 0, 0, time.UTC), false},
		{"09:00 EST", time.Date(2026, 3, 7, 14, 0, 0, 0, time.UTC), true},
		{"08:30 EDT", time.Date(2026, 3, 9, 12, 30, 0, 0, time.UTC), false},
		{"09:30 EDT", time.Date(2026, 3, 9, 13, 30, 0, 0, time.UTC), true},
		{"17:00 EDT", time.Date(2026, 3, 9, 21, 0, 0, 0, time.UTC), false},
	} {
		if st := s.At(tt.utc); st.In != tt.in {
			t.Errorf("%s: In = %v, want %v", tt.name, st.In, tt.in)
		}
	}
	if st := s.At(time.Date(2026, 3, 9, 13, 30, 0, 0, time.UTC)); !st.Until.Equal(time.Date(2026, 3, 9, 21, 0, 0, 0, time.UTC)) {
		t.Errorf("the peak ends at %v, want 21:00 UTC (17:00 EDT)", st.Until)
	}
}

// A provider made by profile add carries its peak hours, and the text it is
// written as loads back.
func TestAddProfileTextWritesPeakHours(t *testing.T) {
	windows := []PeakWindow{{Days: "mon-fri", From: "14:00", To: "18:00"}, {From: "20:00", To: "23:00"}}
	prov := &ProviderSpec{Name: "zai9", Type: ProviderTypeAnthropicCompat, BaseURL: "https://api.z.ai/api/anthropic", Peak: windows, PeakTZ: "Asia/Shanghai"}
	text, err := AddProfileText(peakBase, prov, ProfileSpec{Name: "zai9", Provider: "zai9"})
	if err != nil {
		t.Fatal(err)
	}
	pr := providerPtr(mustLoad(t, text), "zai9")
	if pr == nil || !reflect.DeepEqual(pr.Peak, windows) || pr.PeakTZ != "Asia/Shanghai" {
		t.Errorf("provider after the add: %+v", pr)
	}
	if other := providerPtr(mustLoad(t, text), "zai"); len(other.Peak) != 0 {
		t.Errorf("another provider got peak hours: %+v", other.Peak)
	}
}

// A pause file that cannot be read or holds a bad time pauses nothing; the
// next pause replaces it.
func TestPeakPausesIgnoreAWrongFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, pauseFile)
	p := NewPeakPauses(dir)
	for _, body := range []string{"zai: [\nbroken\n", "zai: not-a-time\n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := p.Until("zai", now); ok {
			t.Errorf("paused by %q", body)
		}
	}
	if err := p.Set("zai", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Until("zai", now); !ok {
		t.Error("a new pause over a wrong file does not hold")
	}
}

func TestParseClock(t *testing.T) {
	for in, want := range map[string]int{"0:00": 0, "00:00": 0, "9:05": 545, "23:59": 1439, "24:00": 1440} {
		if got, err := parseClock(in); err != nil || got != want {
			t.Errorf("parseClock(%q) = %d, %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "9", "24:01", "25:00", "12:60", "12:5", "1:2:3", "-1:00", "ab:cd", "123:00"} {
		if got, err := parseClock(bad); err == nil {
			t.Errorf("parseClock(%q) = %d, want an error", bad, got)
		}
	}
}

// A window may end at midnight, written 24:00 or 00:00.
func TestPeakAtEndsAtMidnight(t *testing.T) {
	for _, to := range []string{"24:00", "00:00"} {
		s := mustSchedule(t, "UTC", PeakWindow{Days: "tue", From: "20:00", To: to})
		if st := s.At(at("UTC", 3, 23, 59)); !st.In || !st.Until.Equal(at("UTC", 4, 0, 0)) {
			t.Errorf("to %s, tuesday 23:59: %+v", to, st)
		}
		if st := s.At(at("UTC", 4, 0, 0)); st.In || !st.Until.Equal(at("UTC", 10, 20, 0)) {
			t.Errorf("to %s, wednesday 00:00: %+v, want the next start on tuesday 20:00", to, st)
		}
	}
}

func TestClockInNamesTheWeekdayOfAnotherDay(t *testing.T) {
	ref := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)
	if got := clockIn(ref.Add(time.Hour), time.UTC, ref); got != "16:00" {
		t.Errorf("same day: %q", got)
	}
	if got := clockIn(ref.Add(24*time.Hour), time.UTC, ref); got != "Wed 15:00" {
		t.Errorf("next day: %q", got)
	}
	// The same day of the year, a year on.
	if got := clockIn(time.Date(2027, 11, 3, 15, 0, 0, 0, time.UTC), time.UTC, ref); got != "Wed 15:00" {
		t.Errorf("a year on: %q", got)
	}
}

func TestZoneNameAndNegativeWait(t *testing.T) {
	w := PeakWindow{From: "14:00", To: "18:00"}
	if got := mustSchedule(t, "", w).ZoneName(); got != "local" {
		t.Errorf("ZoneName of the machine's zone = %q", got)
	}
	if got := mustSchedule(t, "+08:00", w).ZoneName(); got != "+08:00" {
		t.Errorf("ZoneName = %q", got)
	}
	if got := FormatWait(-5 * time.Minute); got != "1m" {
		t.Errorf("FormatWait of a wait that is over = %q", got)
	}
}

func TestSetProviderPeakTextKeepsComments(t *testing.T) {
	base := strings.Replace(peakBase, "        type: anthropic_compat\n",
		"        type: anthropic_compat\n        ## my note about peak hours\n        # peak: [ the old way\n", 1)
	windows := []PeakWindow{{From: "14:00", To: "18:00"}}
	set, err := SetProviderPeakText(base, "zai", windows, "UTC")
	if err != nil || !strings.Contains(set, "## my note about peak hours\n        # peak: [ the old way\n") {
		t.Fatalf("set: %v\n%s", err, set)
	}
	if pr := providerPtr(mustLoad(t, set), "zai"); !reflect.DeepEqual(pr.Peak, windows) {
		t.Errorf("loaded back: %+v", pr.Peak)
	}
	if back, err := SetProviderPeakText(set, "zai", nil, ""); err != nil || back != base {
		t.Errorf("clear: %v\n%s", err, back)
	}
}

func TestSetProviderPeakTextNeedsAProvidersList(t *testing.T) {
	if _, err := SetProviderPeakText("listen.port: 60093\n", "zai", []PeakWindow{{From: "14:00", To: "18:00"}}, ""); err == nil {
		t.Error("a config without providers was edited")
	}
}

// Keys at an indent of their own: the object's first key sets the indent of the
// new lines, and with none of the scalar keys there the lines go to the end of
// the object.
func TestSetProviderPeakTextWithUnevenIndent(t *testing.T) {
	const odd = "providers: [\n    {\n            effort_map: {\n                low: low\n            }\n        name: zai\n        type: anthropic_compat\n        base_url: https://api.z.ai/api/anthropic\n    }\n]\nprofiles: [\n    {\n        name: zai\n        provider: zai\n    }\n]\n"
	windows := []PeakWindow{{Days: "sat", From: "10:00", To: "12:00"}}
	set, err := SetProviderPeakText(odd, "zai", windows, "")
	if err != nil || !strings.HasSuffix(strings.SplitN(set, "profiles:", 2)[0], "            peak: [\n                {\n                    days: sat\n                    from: 10:00\n                    to: 12:00\n                }\n            ]\n    }\n]\n") {
		t.Fatalf("set: %v\n%s", err, set)
	}
	if pr := providerPtr(mustLoad(t, set), "zai"); !reflect.DeepEqual(pr.Peak, windows) {
		t.Errorf("loaded back: %+v", pr.Peak)
	}
	again, err := SetProviderPeakText(set, "zai", []PeakWindow{{From: "01:00", To: "02:00"}}, "")
	if err != nil || strings.Contains(again, "10:00") || strings.Count(again, "peak: [") != 1 {
		t.Errorf("replace: %v\n%s", err, again)
	}
	if back, err := SetProviderPeakText(set, "zai", nil, ""); err != nil || back != odd {
		t.Errorf("clear: %v\n%s", err, back)
	}
}

func TestPeakPausesEdgeCases(t *testing.T) {
	now := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	p := NewPeakPauses(dir)

	// Ending a pause that is not there is not an error, and a pause that ended
	// before now is no pause.
	if err := p.Set("zai", time.Time{}, now); err != nil {
		t.Errorf("ending a missing pause: %v", err)
	}
	if err := p.Set("zai", now.Add(-time.Minute), now); err != nil {
		t.Errorf("a pause in the past: %v", err)
	}
	if _, ok := p.Until("zai", now.Add(-2*time.Minute)); ok {
		t.Error("a pause in the past was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, pauseFile)); err == nil {
		t.Error("a file was written for a pause that is over")
	}

	// The directory is made when it is not there; one that cannot be made is an error.
	nested := filepath.Join(dir, "a", "b")
	if err := NewPeakPauses(nested).Set("zai", now.Add(time.Hour), now); err != nil {
		t.Fatalf("nested directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nested, pauseFile)); err != nil {
		t.Errorf("pause file: %v", err)
	}
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewPeakPauses(file).Set("zai", now.Add(time.Hour), now); err == nil {
		t.Error("a pause was written under a plain file")
	}
}
