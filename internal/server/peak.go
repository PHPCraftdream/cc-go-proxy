package server

import (
	"sort"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

// peakBanner lists, for the start-up output, the providers that have peak hours.
func peakBanner(cfg *config.Config) []string {
	var names []string
	for name, pr := range cfg.Providers {
		if len(pr.Peak) > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		pr := cfg.Providers[name]
		var windows []string
		for _, w := range pr.Peak {
			windows = append(windows, w.String())
		}
		zone := pr.PeakTZ
		if zone == "" {
			zone = "local time"
		}
		out = append(out, "   peak hours of "+name+": "+strings.Join(windows, ", ")+" ("+zone+"); requests are refused then, see ccgp --peak")
	}
	return out
}

// peakRefusal is the answer to a request that arrives in the peak hours of its
// provider (config `peak`), "" when it may go on: no peak hours, outside them, or
// paused with `ccgp --peak <provider> pause`. The answer is an HTTP error, not a
// reply of the model, so Claude Code shows it as a notice of its own and the
// provider never sees the request.
func peakRefusal(cfg *config.Config, pauses *config.PeakPauses, now time.Time) string {
	pr := cfg.Providers[cfg.ProviderName]
	if pr == nil || len(pr.Peak) == 0 {
		return ""
	}
	s, err := pr.PeakSchedule()
	if err != nil || s == nil || !s.InPeak(now) {
		return ""
	}
	if _, paused := pauses.Until(pr.Name, now); paused {
		return ""
	}
	st := s.At(now)
	var windows []string
	for _, w := range st.Windows {
		windows = append(windows, w.String())
	}
	var b strings.Builder
	b.WriteString("Peak hours of provider " + pr.Name + ": " + strings.Join(windows, ", ") + " (" + s.ZoneName() + " time). ")
	if st.Until.IsZero() {
		b.WriteString("Requests are refused for as long as the peak hours cover all time. ")
	} else {
		b.WriteString("Requests resume at " + s.ClockText(st.Until, now) + ". ")
	}
	b.WriteString("To work anyway: ccgp --peak " + pr.Name + " pause")
	return b.String()
}
