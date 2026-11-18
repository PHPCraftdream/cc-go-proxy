package tune

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	peakcfg "github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

// providerText describes one provider, the text `ccgp --provider NAME` prints
// and the interface shows above a provider's menu.
func ProviderText(cfg *config.Config, store creds.Store, pauses *peakcfg.Pauses, now time.Time, name string) string {
	pr, ok := cfg.Providers[name]
	if !ok {
		return "this provider is gone"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "type         %s\n", pr.Type)
	if pr.Type != config.ProviderTypeClaude {
		base := common.Dash(pr.EffectiveBaseURL())
		if pr.Type == config.ProviderTypeClaudeAPI && pr.BaseURL == "" {
			base += " (the default)"
		}
		fmt.Fprintf(&b, "base_url     %s\napi_key_env  %s\n", base, common.Dash(pr.APIKeyEnv))
	}
	fmt.Fprintf(&b, "login        %s\n", common.LoginState(store, pr))
	if !config.IsOwnType(pr.Type) {
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
	if !config.IsOwnType(pr.Type) {
		fmt.Fprintf(&b, "peak hours   %s\n", PeakSummary(pr, name, now, pauses))
	}
	fmt.Fprintf(&b, "profiles     %s", common.Dash(strings.Join(common.ProfilesOf(cfg, name), ", ")))
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
func PeakSummary(pr *config.Provider, name string, now time.Time, pauses *peakcfg.Pauses) string {
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
