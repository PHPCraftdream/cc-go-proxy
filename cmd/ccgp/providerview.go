package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

// providerText describes one provider, the text `ccgp --provider NAME` prints
// and the interface shows above a provider's menu.
func providerText(cfg *config.Config, store creds.Store, pauses *config.PeakPauses, now time.Time, name string) string {
	pr, ok := cfg.Providers[name]
	if !ok {
		return "this provider is gone"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "type         %s\n", pr.Type)
	if pr.Type != config.ProviderTypeClaude {
		fmt.Fprintf(&b, "base_url     %s\napi_key_env  %s\n", dash(pr.BaseURL), dash(pr.APIKeyEnv))
	}
	fmt.Fprintf(&b, "login        %s\n", loginState(store, pr))
	if pr.Type != config.ProviderTypeClaude {
		fmt.Fprintf(&b, "proxy        %s\n", providerProxyText(cfg, pr))
	}
	if len(pr.EffortMap) > 0 {
		keys := make([]string, 0, len(pr.EffortMap))
		for k := range pr.EffortMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+pr.EffortMap[k])
		}
		fmt.Fprintf(&b, "effort_map   %s\n", strings.Join(parts, " "))
	}
	if pr.Type != config.ProviderTypeClaude {
		fmt.Fprintf(&b, "peak hours   %s\n", peakSummary(pr, name, now, pauses))
	}
	fmt.Fprintf(&b, "profiles     %s", dash(strings.Join(profilesOf(cfg, name), ", ")))
	return b.String()
}

// providerProxyText says which proxy a provider's profiles go through unless
// they have their own, and where it comes from.
func providerProxyText(cfg *config.Config, pr *config.Provider) string {
	choice := cfg.ProviderProxy(pr.Name).String()
	if pr.Proxy != "" {
		return choice + " (its own)"
	}
	return "global (" + choice + ")"
}

// peakSummary sums up a provider's peak hours and whether it is peak now.
func peakSummary(pr *config.Provider, name string, now time.Time, pauses *config.PeakPauses) string {
	s, err := pr.PeakSchedule()
	if err != nil {
		return "unreadable: " + err.Error()
	}
	if s == nil {
		return "none"
	}
	windows := make([]string, 0, len(s.Windows))
	for _, w := range s.Windows {
		windows = append(windows, w.String())
	}
	state := "open now"
	if st := s.At(now); st.In {
		state = "PEAK NOW"
		if _, paused := pauses.Until(name, now); paused {
			state = "peak now, paused"
		}
	}
	return fmt.Sprintf("%s (%s time); %s", strings.Join(windows, ", "), s.ZoneName(), state)
}
