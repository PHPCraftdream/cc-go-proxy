package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // zone names work on machines without zoneinfo (Windows)
)

// PeakWindow is one stretch of a provider's peak hours as config.ktav holds it:
//
//	{ days: mon-fri   from: 14:00   to: 18:00 }
//
// days is empty (every day), a list of weekdays `sat,sun`, a range `mon-fri`
// (it may wrap past the end of the week: `fri-mon`), or both mixed. A window
// whose to is not after from runs past midnight (`22:00` to `06:00`); its days
// are the days it starts on. to is exclusive, `24:00` is the end of the day.
type PeakWindow struct {
	Days string `json:"days"`
	From string `json:"from"`
	To   string `json:"to"`
}

// String is the window in the form `ccgp --peak` takes: `mon-fri 14:00-18:00`.
func (w PeakWindow) String() string {
	if w.Days == "" {
		return w.From + "-" + w.To
	}
	return w.Days + " " + w.From + "-" + w.To
}

var weekdayNames = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

func weekdayIndex(name string) (int, bool) {
	for i, n := range weekdayNames {
		if n == name {
			return i, true
		}
	}
	return 0, false
}

// parseDays turns `mon-fri,sun` into a bit set (bit i: time.Weekday i); ""
// is every day.
func parseDays(s string) (uint8, error) {
	if s == "" {
		return 0x7f, nil
	}
	var set uint8
	for _, part := range strings.Split(strings.ToLower(s), ",") {
		from, to, isRange := strings.Cut(part, "-")
		a, ok := weekdayIndex(from)
		if !ok {
			return 0, fmt.Errorf("unknown day %q (mon tue wed thu fri sat sun, a range mon-fri, a list sat,sun)", part)
		}
		if !isRange {
			set |= 1 << a
			continue
		}
		b, ok := weekdayIndex(to)
		if !ok {
			return 0, fmt.Errorf("unknown day %q (mon tue wed thu fri sat sun, a range mon-fri, a list sat,sun)", part)
		}
		for d := a; ; d = (d + 1) % 7 {
			set |= 1 << d
			if d == b {
				break
			}
		}
	}
	return set, nil
}

// parseClock reads `H:MM` / `HH:MM` as minutes since midnight; 24:00 is 1440.
func parseClock(s string) (int, error) {
	if m := clockRe.FindStringSubmatch(s); m != nil {
		hh, _ := strconv.Atoi(m[1])
		mm, _ := strconv.Atoi(m[2])
		if mm < 60 && (hh < 24 || hh == 24 && mm == 0) {
			return hh*60 + mm, nil
		}
	}
	return 0, fmt.Errorf("bad time %q: want HH:MM from 00:00 to 24:00", s)
}

var clockRe = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)

func formatClock(min int) string { return fmt.Sprintf("%02d:%02d", min/60, min%60) }

// peakSpan is a parsed PeakWindow.
type peakSpan struct {
	days     uint8
	from, to int // minutes since midnight; to <= from: past midnight
}

func (w PeakWindow) span() (peakSpan, error) {
	days, err := parseDays(w.Days)
	if err != nil {
		return peakSpan{}, err
	}
	from, err := parseClock(w.From)
	if err != nil {
		return peakSpan{}, err
	}
	to, err := parseClock(w.To)
	if err != nil {
		return peakSpan{}, err
	}
	if from == to {
		return peakSpan{}, fmt.Errorf("window %s-%s is empty (a whole day is 00:00-24:00)", w.From, w.To)
	}
	if from == 1440 {
		return peakSpan{}, fmt.Errorf("a window cannot start at 24:00 (use 00:00)")
	}
	return peakSpan{days: days, from: from, to: to}, nil
}

// covers reports whether the span holds the wall-clock moment t.
func (s peakSpan) covers(t time.Time) bool {
	day := int(t.Weekday())
	sec := t.Hour()*3600 + t.Minute()*60 + t.Second()
	if s.to > s.from {
		return s.days&(1<<day) != 0 && sec >= s.from*60 && sec < s.to*60
	}
	yesterday := (day + 6) % 7
	return (s.days&(1<<day) != 0 && sec >= s.from*60) || (s.days&(1<<yesterday) != 0 && sec < s.to*60)
}

var clockRangeRe = regexp.MustCompile(`^\d{1,2}:\d{2}-\d{1,2}:\d{2}$`)

// ParsePeakWords reads windows as `ccgp --peak` takes them: for each one an
// optional days word and then `HH:MM-HH:MM`, e.g. `mon-fri 14:00-18:00 20:00-23:00`.
// A word may hold a whole window in quotes. The windows come back normalized.
func ParsePeakWords(words []string) ([]PeakWindow, error) {
	var out []PeakWindow
	var days []string
	for _, w := range strings.Fields(strings.Join(words, " ")) {
		if !clockRangeRe.MatchString(w) {
			days = append(days, w)
			continue
		}
		if len(days) > 1 {
			return nil, fmt.Errorf("%q: give the days as one word without spaces (mon,tue or mon-fri)", strings.Join(days, " "))
		}
		from, to, _ := strings.Cut(w, "-")
		pw := PeakWindow{From: from, To: to}
		if len(days) == 1 {
			pw.Days = days[0]
		}
		var err error
		if pw, err = pw.normalized(); err != nil {
			return nil, err
		}
		out = append(out, pw)
		days = nil
	}
	if len(days) > 0 {
		return nil, fmt.Errorf("%q has no time range (HH:MM-HH:MM)", strings.Join(days, " "))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no window given: want [days] HH:MM-HH:MM, e.g. mon-fri 14:00-18:00")
	}
	return out, nil
}

// normalized validates w and returns it in the canonical spelling.
func (w PeakWindow) normalized() (PeakWindow, error) {
	if _, err := w.span(); err != nil {
		return w, err
	}
	from, _ := parseClock(w.From)
	to, _ := parseClock(w.To)
	return PeakWindow{Days: strings.ToLower(w.Days), From: formatClock(from), To: formatClock(to)}, nil
}

// zoneOffset reads a fixed offset (+08:00, +8, -0530, UTC+8, GMT-3): its seconds
// east of UTC and its canonical spelling (+08:00). ok is false when s is not an
// offset at all.
func zoneOffset(s string) (sec int, canon string, ok bool, err error) {
	off := s
	for _, p := range []string{"UTC", "utc", "GMT", "gmt"} {
		off = strings.TrimPrefix(off, p)
	}
	if off == "" || (off[0] != '+' && off[0] != '-') {
		return 0, "", false, nil
	}
	digits := strings.Replace(off[1:], ":", "", 1)
	if len(digits) <= 2 {
		digits += "00"
	}
	if len(digits) == 3 {
		digits = "0" + digits
	}
	hh, herr := strconv.Atoi(digits[:len(digits)-2])
	mm, merr := strconv.Atoi(digits[len(digits)-2:])
	if len(digits) > 4 || herr != nil || merr != nil || hh > 14 || mm > 59 {
		return 0, "", true, fmt.Errorf("bad time zone %q: want a name like Asia/Shanghai or an offset like +08:00", s)
	}
	sec = hh*3600 + mm*60
	if off[0] == '-' {
		sec = -sec
	}
	return sec, fmt.Sprintf("%c%02d:%02d", off[0], hh, mm), true, nil
}

// ParsePeakZone reads a time zone: an IANA name (Asia/Shanghai), a fixed offset
// (+08:00, +8, -0530, UTC+8, GMT-3), UTC, or "" for the machine's own.
func ParsePeakZone(s string) (*time.Location, error) {
	if s == "" {
		return time.Local, nil
	}
	if strings.EqualFold(s, "utc") || strings.EqualFold(s, "gmt") {
		return time.UTC, nil
	}
	if sec, canon, ok, err := zoneOffset(s); ok {
		if err != nil {
			return nil, err
		}
		return time.FixedZone(canon, sec), nil
	}
	loc, err := time.LoadLocation(s)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q: want a name like Asia/Shanghai or an offset like +08:00", s)
	}
	return loc, nil
}

// CanonicalPeakZone checks a zone and returns the text to keep in config.ktav:
// an offset as +08:00 (a bare +8 would read as a number there), UTC as UTC,
// a name as given.
func CanonicalPeakZone(s string) (string, error) {
	if _, err := ParsePeakZone(s); err != nil {
		return "", err
	}
	if strings.EqualFold(s, "utc") || strings.EqualFold(s, "gmt") {
		return "UTC", nil
	}
	if _, canon, ok, _ := zoneOffset(s); ok {
		return canon, nil
	}
	return s, nil
}

// PeakSchedule is the peak hours of one provider.
type PeakSchedule struct {
	Windows []PeakWindow
	TZ      string // as written; "" is the machine's zone
	Loc     *time.Location
	spans   []peakSpan
}

// NewPeakSchedule checks the windows and the zone. No windows: nil.
func NewPeakSchedule(windows []PeakWindow, tz string) (*PeakSchedule, error) {
	if len(windows) == 0 {
		if tz != "" {
			return nil, fmt.Errorf("peak_tz is set but there are no peak windows")
		}
		return nil, nil
	}
	loc, err := ParsePeakZone(tz)
	if err != nil {
		return nil, err
	}
	s := &PeakSchedule{Windows: windows, TZ: tz, Loc: loc}
	for i, w := range windows {
		sp, err := w.span()
		if err != nil {
			return nil, fmt.Errorf("peak[%d]: %w", i, err)
		}
		s.spans = append(s.spans, sp)
	}
	return s, nil
}

// PeakSchedule is the provider's peak hours; nil when it has none.
func (p *Provider) PeakSchedule() (*PeakSchedule, error) {
	return NewPeakSchedule(p.Peak, p.PeakTZ)
}

func (s *PeakSchedule) covers(t time.Time) bool {
	t = t.In(s.Loc)
	for _, sp := range s.spans {
		if sp.covers(t) {
			return true
		}
	}
	return false
}

// peakLookahead bounds the search for the next change: a week and a day, so a
// weekly window is always found.
const peakLookahead = 8 * 24 * 60

// PeakState is what a schedule says about a moment.
type PeakState struct {
	In bool
	// Until is when the peak that holds the moment ends (runs of touching windows
	// count as one); When not In it is when the next one starts. Zero: it never
	// does (a schedule that covers all time, or one that cannot be reached).
	Until time.Time
	// Windows are the windows holding the moment (In) or starting at Until.
	Windows []PeakWindow
}

// At reports whether now is in peak hours and when that changes, to the minute.
func (s *PeakSchedule) At(now time.Time) PeakState {
	in := s.covers(now)
	st := PeakState{In: in}
	t := now.Truncate(time.Minute)
	for i := 0; i < peakLookahead; i++ {
		t = t.Add(time.Minute)
		if s.covers(t) != in {
			st.Until = t
			break
		}
	}
	if in {
		st.Windows = s.windowsAt(now)
	} else if !st.Until.IsZero() {
		st.Windows = s.windowsAt(st.Until)
	}
	return st
}

func (s *PeakSchedule) windowsAt(t time.Time) []PeakWindow {
	t = t.In(s.Loc)
	var out []PeakWindow
	for i, sp := range s.spans {
		if sp.covers(t) {
			out = append(out, s.Windows[i])
		}
	}
	return out
}

// InPeak is the cheap half of At: whether now is in peak hours.
func (s *PeakSchedule) InPeak(now time.Time) bool { return s.covers(now) }

// ClockText says when t is, for a person who reads it at now: the clock in the
// schedule's zone and, when that is not the machine's zone, the machine's clock
// too, then how long it is to wait. A weekday name goes in front of a clock that
// is not today's. "18:00 Asia/Shanghai (13:00 your time, in 2h 18m)".
func (s *PeakSchedule) ClockText(t, now time.Time) string {
	wait := "in " + FormatWait(t.Sub(now))
	if s.TZ == "" {
		return clockIn(t, time.Local, now) + " (" + wait + ")"
	}
	return clockIn(t, s.Loc, now) + " " + s.TZ + " (" + clockIn(t, time.Local, now) + " your time, " + wait + ")"
}

func clockIn(t time.Time, loc *time.Location, ref time.Time) string {
	t, ref = t.In(loc), ref.In(loc)
	if t.YearDay() != ref.YearDay() || t.Year() != ref.Year() {
		return t.Format("Mon 15:04")
	}
	return t.Format("15:04")
}

// FormatWait writes a wait as "2h 18m", "45m" or "1d 4h"; under a minute it is
// "1m".
func FormatWait(d time.Duration) string {
	m := int((d + 59*time.Second) / time.Minute) // round up: never promise less
	switch {
	case m < 1:
		return "1m"
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m < 24*60:
		if m%60 == 0 {
			return fmt.Sprintf("%dh", m/60)
		}
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	}
	if h := (m % (24 * 60)) / 60; h > 0 {
		return fmt.Sprintf("%dd %dh", m/(24*60), h)
	}
	return fmt.Sprintf("%dd", m/(24*60))
}

// ZoneName is the zone as people read it: the name given, or "local".
func (s *PeakSchedule) ZoneName() string {
	if s.TZ == "" {
		return "local"
	}
	return s.TZ
}

// validatePeak checks a provider's peak settings for the loader.
func validatePeak(pr Provider) error {
	if IsOwnType(pr.Type) {
		if len(pr.Peak) > 0 || pr.PeakTZ != "" {
			return fmt.Errorf("provider %q: type %q takes no peak hours (it does not go through ccgp)", pr.Name, pr.Type)
		}
		return nil
	}
	if _, err := pr.PeakSchedule(); err != nil {
		return fmt.Errorf("provider %q: %w", pr.Name, err)
	}
	return nil
}
