package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

// The commands that set one key of a profile or a provider: the twins of the
// fields of the interface (`ccgp --interactive`), which make the same edit.

const (
	profileSetUsage = "usage: ccgp --profile set <name> <key> <value> | ccgp --profile unset <name> <key>   (keys: default_model, context_window, compact_percent, provider; unset does not take provider)"
	providerUsage   = "usage: ccgp --provider [NAME] [--json] | ccgp --provider set <name> <key> <value> | ccgp --provider unset <name> <key>   (keys: base_url, api_key_env)"
)

// reportEdit says what an edit of config.ktav did, as the other commands do.
func reportEdit(w io.Writer, backup string) {
	if backup == "" {
		_, _ = fmt.Fprintln(w, "config.ktav already had this value")
		return
	}
	_, _ = fmt.Fprintf(w, "config.ktav updated (old one saved as %s); the running server applies it on its next request\n", backup)
}

// profileSet implements `ccgp --profile set|unset <name> <key> [value]`.
func profileSet(w io.Writer, cfg *config.Config, args []string, unset bool) error {
	want := 3
	if unset {
		want = 2
	}
	if len(args) != want {
		return errors.New(profileSetUsage)
	}
	name, key := args[0], args[1]
	pf, ok := cfg.Profiles[name]
	if !ok {
		return fmt.Errorf("unknown profile %q (configured: %s)", name, names(cfg.Profiles))
	}
	if isClaudeProfile(cfg, pf) {
		return fmt.Errorf("profile %q is a Claude profile: it does not go through ccgp, so it has no settings of ccgp's", name)
	}
	if !slices.Contains(config.ProfileKeys(), key) {
		return fmt.Errorf("unknown profile key %q (%s)", key, strings.Join(config.ProfileKeys(), ", "))
	}
	value := ""
	if !unset {
		value = strings.TrimSpace(args[2])
	}
	switch key {
	case "provider":
		if unset || value == "" {
			return errors.New("a profile needs a provider")
		}
		if !slices.Contains(providerNames(cfg), value) {
			return fmt.Errorf("unknown provider %q (configured: %s)", value, strings.Join(providerNames(cfg), ", "))
		}
	case "context_window", "compact_percent":
		if value != "" {
			if n, err := strconv.ParseInt(value, 10, 64); err != nil || n < 0 {
				return fmt.Errorf("%s: want a number, got %q", key, value)
			}
		}
	}
	if cfg.IsEnvMode() {
		return errors.New("no config.ktav (env-only mode): nothing to edit")
	}
	backup, err := config.EditFile(cfg.ConfigFile, func(text string) (string, error) {
		return config.SetProfileKeyText(text, name, key, value)
	})
	if err != nil {
		return err
	}
	reportEdit(w, backup)
	return nil
}

// providerRow is one provider in `ccgp --provider --json`.
type providerRow struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	BaseURL   string   `json:"baseUrl"`
	APIKeyEnv string   `json:"apiKeyEnv"`
	Login     string   `json:"login"`
	Proxy     string   `json:"proxy"`
	ProxyOwn  bool     `json:"proxyOwn"`
	PeakHours string   `json:"peakHours"`
	Profiles  []string `json:"profiles"`
}

type providerListData struct {
	Providers []providerRow `json:"providers"`
}

func providerRows(cfg *config.Config, store creds.Store, pauses *config.PeakPauses, now time.Time) []providerRow {
	var rows []providerRow
	for _, name := range sortedKeys(cfg.Providers) {
		pr := cfg.Providers[name]
		row := providerRow{Name: name, Type: pr.Type, BaseURL: pr.EffectiveBaseURL(), APIKeyEnv: pr.APIKeyEnv,
			Login: loginState(store, pr), Profiles: profilesOf(cfg, name)}
		if !config.IsOwnType(pr.Type) {
			row.Proxy, row.ProxyOwn = cfg.ProviderProxy(name).String(), pr.Proxy != ""
			row.PeakHours = peakSummary(pr, name, now, pauses)
		}
		rows = append(rows, row)
	}
	return rows
}

func providerCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := credsStore()
	if err != nil {
		return err
	}
	return provider(os.Stdout, cfg, store, config.NewPeakPauses(store.Dir), time.Now(), args)
}

// provider implements `ccgp --provider`: the list, one provider in detail, and
// set / unset of base_url and api_key_env.
func provider(w io.Writer, cfg *config.Config, store creds.Store, pauses *config.PeakPauses, now time.Time, args []string) error {
	if cfg.IsEnvMode() {
		return errors.New("ccgp --provider needs config.ktav (env-only mode has a single implicit provider)")
	}
	if len(args) > 0 && (args[0] == "set" || args[0] == "unset") {
		return providerSet(w, cfg, args[1:], args[0] == "unset")
	}
	jsonOut, name := false, ""
	for _, a := range args {
		switch {
		case a == "--json":
			jsonOut = true
		case name == "" && !strings.HasPrefix(a, "-"):
			name = a
		default:
			return errors.New(providerUsage)
		}
	}
	if name != "" {
		if _, ok := cfg.Providers[name]; !ok {
			return fmt.Errorf("unknown provider %q (configured: %s)", name, names(cfg.Providers))
		}
	}
	rows := providerRows(cfg, store, pauses, now)
	if name != "" {
		kept := rows[:0]
		for _, r := range rows {
			if r.Name == name {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if jsonOut {
		return writeJSON(w, providerListData{Providers: rows})
	}
	if name != "" {
		_, _ = fmt.Fprintln(w, providerText(cfg, store, pauses, now, name))
		return nil
	}
	_, _ = fmt.Fprintf(w, "%-14s %-17s %-12s %s\n", "PROVIDER", "TYPE", "PEAK", "PROFILES")
	for _, r := range rows {
		peak := "-"
		if r.PeakHours != "" && r.PeakHours != "none" {
			peak = "yes"
		}
		_, _ = fmt.Fprintf(w, "%-14s %-17s %-12s %s\n", r.Name, r.Type, peak, dash(strings.Join(r.Profiles, ", ")))
	}
	return nil
}

// providerSet implements `ccgp --provider set|unset <name> <key> [value]`.
func providerSet(w io.Writer, cfg *config.Config, args []string, unset bool) error {
	want := 3
	if unset {
		want = 2
	}
	if len(args) != want {
		return errors.New(providerUsage)
	}
	name, key := args[0], args[1]
	pr, ok := cfg.Providers[name]
	if !ok {
		return fmt.Errorf("unknown provider %q (configured: %s)", name, names(cfg.Providers))
	}
	if pr.Type == config.ProviderTypeClaude {
		return fmt.Errorf("provider %q is a Claude profile: it does not go through ccgp, so it has no settings of ccgp's", name)
	}
	if !slices.Contains(config.ProviderKeys(), key) {
		return fmt.Errorf("unknown provider key %q (%s)", key, strings.Join(config.ProviderKeys(), ", "))
	}
	if key == "api_key_env" && pr.Type == config.ProviderTypeCodex {
		return fmt.Errorf("provider %q signs in with OAuth: it takes no api_key_env", name)
	}
	value := ""
	if !unset {
		value = strings.TrimSpace(args[2])
	}
	backup, err := config.EditFile(cfg.ConfigFile, func(text string) (string, error) {
		return config.SetProviderKeyText(text, name, key, value)
	})
	if err != nil {
		return err
	}
	reportEdit(w, backup)
	return nil
}

// profileVerbs and providerVerbs list the entry points of --profile and
// --provider beyond the command itself.
func profileVerbs() []string {
	v := []string{"list", "add --like", "add --provider", "add --type", "rm"}
	for _, k := range config.ProfileKeys() {
		v = append(v, "set "+k)
		if k != "provider" {
			v = append(v, "unset "+k)
		}
	}
	return v
}

func providerVerbs() []string {
	var v []string
	for _, k := range config.ProviderKeys() {
		v = append(v, "set "+k, "unset "+k)
	}
	return v
}
