package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

const peakUsage = "usage: ccgp --peak [provider [set|add WINDOW... | rm N... | tz ZONE | clear | pause [DURATION] | resume]] [--json]"

// maxPeakPause bounds a pause: a longer one is a cleared schedule.
const maxPeakPause = 7 * 24 * time.Hour

func peakCmd(args []string) error {
	dir, err := creds.DefaultDir()
	if err != nil {
		return err
	}
	return peak(os.Stdout, time.Now(), config.NewPeakPauses(dir), args)
}

// peak shows the peak hours of the providers, or changes those of one in
// config.ktav (the running server applies it on its next request); pause and
// resume go to the pause file instead.
func peak(w io.Writer, now time.Time, pauses *config.PeakPauses, args []string) error {
	jsonOut := false
	var rest []string
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		} else {
			rest = append(rest, a)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return showPeak(w, cfg, now, pauses, "", jsonOut)
	}
	provider, err := peakProvider(cfg, rest[0])
	if err != nil {
		return err
	}
	if len(rest) == 1 {
		return showPeak(w, cfg, now, pauses, provider, jsonOut)
	}
	pr := cfg.Providers[provider]
	verb, words := rest[1], rest[2:]
	switch verb {
	case "set", "add":
		windows, err := config.ParsePeakWords(words)
		if err != nil {
			return err
		}
		if verb == "add" {
			windows = appendNew(pr.Peak, windows)
		}
		return changePeak(w, cfg, now, pauses, provider, windows, pr.PeakTZ)
	case "rm":
		windows, err := removeWindows(pr.Peak, words)
		if err != nil {
			return err
		}
		tz := pr.PeakTZ
		if len(windows) == 0 {
			tz = ""
		}
		return changePeak(w, cfg, now, pauses, provider, windows, tz)
	case "tz":
		if len(words) != 1 {
			return errors.New("usage: ccgp --peak <provider> tz ZONE (a name like Asia/Shanghai, an offset like +08:00, UTC, or local)")
		}
		if len(pr.Peak) == 0 {
			return fmt.Errorf("provider %q has no peak hours yet: add windows first (ccgp --peak %s set ...)", provider, provider)
		}
		tz := ""
		if words[0] != "local" {
			if tz, err = config.CanonicalPeakZone(words[0]); err != nil {
				return err
			}
		}
		return changePeak(w, cfg, now, pauses, provider, pr.Peak, tz)
	case "clear":
		if len(words) != 0 {
			return errors.New(peakUsage)
		}
		return changePeak(w, cfg, now, pauses, provider, nil, "")
	case "pause":
		return pausePeak(w, cfg, now, pauses, provider, words)
	case "resume":
		if len(words) != 0 {
			return errors.New(peakUsage)
		}
		if err := pauses.Set(provider, time.Time{}, now); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(w, "peak hours of %s apply again\n", provider)
		return nil
	}
	return errors.New(peakUsage)
}

// peakProvider turns a provider name (or the name of a profile, which stands
// for its provider) into a provider that can have peak hours.
func peakProvider(cfg *config.Config, name string) (string, error) {
	pr, ok := cfg.Providers[name]
	if !ok {
		pf, isProfile := cfg.Profiles[name]
		if !isProfile {
			return "", fmt.Errorf("unknown provider %q (configured: %s)", name, names(cfg.Providers))
		}
		name, pr = pf.Provider, cfg.Providers[pf.Provider]
	}
	if pr == nil || config.IsOwnType(pr.Type) {
		return "", fmt.Errorf("provider %q is a Claude profile: it does not go through ccgp, so it has no peak hours", name)
	}
	return name, nil
}

// appendNew adds the windows that the list does not hold yet.
func appendNew(have, add []config.PeakWindow) []config.PeakWindow {
	out := append([]config.PeakWindow(nil), have...)
	for _, a := range add {
		dup := false
		for _, h := range out {
			dup = dup || h == a
		}
		if !dup {
			out = append(out, a)
		}
	}
	return out
}

// removeWindows drops the windows numbered as `ccgp --peak` lists them (from 1).
func removeWindows(have []config.PeakWindow, words []string) ([]config.PeakWindow, error) {
	if len(words) == 0 {
		return nil, errors.New("usage: ccgp --peak <provider> rm N... (the numbers of the windows, as ccgp --peak <provider> lists them)")
	}
	drop := map[int]bool{}
	for _, w := range words {
		n, err := strconv.Atoi(w)
		if err != nil || n < 1 || n > len(have) {
			return nil, fmt.Errorf("no window %q (there are %d)", w, len(have))
		}
		drop[n-1] = true
	}
	var out []config.PeakWindow
	for i, h := range have {
		if !drop[i] {
			out = append(out, h)
		}
	}
	return out, nil
}

// changePeak writes the provider's peak hours into config.ktav and shows them.
func changePeak(w io.Writer, cfg *config.Config, now time.Time, pauses *config.PeakPauses, provider string, windows []config.PeakWindow, tz string) error {
	if cfg.IsEnvMode() {
		return errors.New("no config.ktav (env-only mode): nothing to edit")
	}
	backup, err := config.EditFile(cfg.ConfigFile, func(text string) (string, error) {
		return config.SetProviderPeakText(text, provider, windows, tz)
	})
	if err != nil {
		return err
	}
	if backup != "" {
		_, _ = fmt.Fprintf(w, "config.ktav updated (old one saved as %s); the running server applies it on its next request\n", backup)
	} else {
		_, _ = fmt.Fprintln(w, "config.ktav already had these peak hours")
	}
	if cfg, err = config.Load(); err != nil {
		return err
	}
	return showPeak(w, cfg, now, pauses, provider, false)
}

// pausePeak lets requests through for a while in peak hours: for the given
// duration, or till the end of the peak that is running.
func pausePeak(w io.Writer, cfg *config.Config, now time.Time, pauses *config.PeakPauses, provider string, words []string) error {
	s, err := cfg.Providers[provider].PeakSchedule()
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("provider %q has no peak hours, nothing to pause", provider)
	}
	var until time.Time
	switch len(words) {
	case 0:
		st := s.At(now)
		if !st.In {
			return fmt.Errorf("it is not peak time for %s now: give the length of the pause (ccgp --peak %s pause 2h)", provider, provider)
		}
		if st.Until.IsZero() {
			return fmt.Errorf("the peak hours of %s cover all time: give the length of the pause (ccgp --peak %s pause 2h)", provider, provider)
		}
		until = st.Until
	case 1:
		d, err := parsePause(words[0])
		if err != nil {
			return err
		}
		until = now.Add(d)
	default:
		return errors.New(peakUsage)
	}
	if err := pauses.Set(provider, until, now); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "peak hours of %s do not refuse requests until %s\n", provider, s.ClockText(until, now))
	return nil
}

// parsePause reads 90m, 2h, 1h30m or 1d.
func parsePause(s string) (time.Duration, error) {
	bad := fmt.Errorf("bad length of a pause %q: want something like 90m, 2h or 1d", s)
	tooLong := fmt.Errorf("a pause of %s is too long (at most 7d): clear the peak hours instead", s)
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, bad
		}
		if n > 7 {
			return 0, tooLong
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil || d <= 0 {
			return 0, bad
		}
	}
	if d > maxPeakPause {
		return 0, tooLong
	}
	return d, nil
}

// peakInfo is the JSON form of one provider's peak hours.
type peakInfo struct {
	Windows     []config.PeakWindow `json:"windows"`
	Zone        string              `json:"zone"`
	PeakNow     bool                `json:"peak_now"`
	Until       string              `json:"until,omitempty"`
	PausedUntil string              `json:"paused_until,omitempty"`
}

func showPeak(w io.Writer, cfg *config.Config, now time.Time, pauses *config.PeakPauses, only string, jsonOut bool) error {
	var providers []string
	if only != "" {
		providers = []string{only}
	} else {
		for _, name := range sortedKeys(cfg.Providers) {
			if !config.IsOwnType(cfg.Providers[name].Type) {
				providers = append(providers, name)
			}
		}
	}
	all := map[string]peakInfo{}
	for _, name := range providers {
		s, err := cfg.Providers[name].PeakSchedule()
		if err != nil {
			return err
		}
		if s == nil {
			all[name] = peakInfo{Windows: []config.PeakWindow{}}
			if !jsonOut {
				_, _ = fmt.Fprintf(w, "%s: no peak hours\n", name)
			}
			continue
		}
		st := s.At(now)
		info := peakInfo{Windows: s.Windows, Zone: s.ZoneName(), PeakNow: st.In}
		if !st.Until.IsZero() {
			info.Until = st.Until.Format(time.RFC3339)
		}
		paused, isPaused := pauses.Until(name, now)
		if isPaused {
			info.PausedUntil = paused.Format(time.RFC3339)
		}
		all[name] = info
		if !jsonOut {
			printPeak(w, name, s, st, now, paused, isPaused)
		}
	}
	if jsonOut {
		return writeJSON(w, all)
	}
	return nil
}

func printPeak(w io.Writer, name string, s *config.PeakSchedule, st config.PeakState, now, paused time.Time, isPaused bool) {
	zone := s.TZ
	if zone == "" {
		zone = "local time"
	}
	_, _ = fmt.Fprintf(w, "%s: peak hours in %s (there it is %s now)\n", name, zone, now.In(s.Loc).Format("Mon 15:04"))
	for i, win := range s.Windows {
		_, _ = fmt.Fprintf(w, "  %d  %s\n", i+1, win)
	}
	switch {
	case st.In && isPaused:
		_, _ = fmt.Fprintf(w, "  now: peak, but paused: requests go through until %s\n", s.ClockText(paused, now))
	case st.In && st.Until.IsZero():
		_, _ = fmt.Fprintln(w, "  now: PEAK, requests are refused (the windows cover all time)")
	case st.In:
		_, _ = fmt.Fprintf(w, "  now: PEAK, requests are refused until %s\n", s.ClockText(st.Until, now))
	case st.Until.IsZero():
		_, _ = fmt.Fprintln(w, "  now: open")
	default:
		_, _ = fmt.Fprintf(w, "  now: open, the next peak starts %s\n", s.ClockText(st.Until, now))
	}
	if isPaused && !st.In {
		_, _ = fmt.Fprintf(w, "  paused until %s\n", s.ClockText(paused, now))
	}
}
