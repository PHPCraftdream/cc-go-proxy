package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

const connectionsUsage = "usage: ccgp --connections [active N] [total N] [proxy URL|off [provider NAME | profile NAME]] [--json]"

func connectionsCmd(args []string) error { return connections(os.Stdout, args) }

// connections shows connections.active / .total / .proxy, or sets them in
// config.ktav (the running server applies them on its next request).
func connections(w io.Writer, args []string) error {
	jsonOut := false
	set := map[string]int64{}
	var order []string
	proxy, setProxy := "", false
	scope, scopeName := "", "" // "provider" or "profile" and its name; "": the global proxy
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--json":
			jsonOut = true
		case "active", "total":
			if i+1 >= len(args) {
				return errors.New(connectionsUsage)
			}
			n, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil || n < 0 {
				return fmt.Errorf("%s: want a number (0: default), got %q", a, args[i+1])
			}
			if _, dup := set[a]; !dup {
				order = append(order, a)
			}
			set[a] = n
			i++
		case "proxy":
			if i+1 >= len(args) {
				return errors.New(connectionsUsage)
			}
			proxy, setProxy = args[i+1], true
			i++
			if i+2 < len(args) && (args[i+1] == "profile" || args[i+1] == "provider") {
				scope, scopeName = args[i+1], args[i+2]
				i += 2
			}
		default:
			return errors.New(connectionsUsage)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if setProxy {
		if scope == "provider" {
			proxy, err = providerProxyValue(cfg, proxy, scopeName)
		} else {
			proxy, err = proxyValue(cfg, proxy, scopeName)
		}
		if err != nil {
			return err
		}
	}
	backup := ""
	if len(set) > 0 || setProxy {
		if cfg.IsEnvMode() {
			return errors.New("no config.ktav (env-only mode): nothing to edit")
		}
		backup, err = config.EditFile(cfg.ConfigFile, func(text string) (string, error) {
			for _, k := range order {
				var err error
				if text, err = config.SetConnectionsText(text, k, set[k]); err != nil {
					return "", err
				}
			}
			switch {
			case setProxy && scope == "provider":
				return config.SetProviderProxyText(text, scopeName, proxy)
			case setProxy && scope == "profile":
				return config.SetProfileProxyText(text, scopeName, proxy)
			case setProxy:
				return config.SetConnectionsProxyText(text, proxy)
			}
			return text, nil
		})
		if err != nil {
			return err
		}
		if cfg, err = config.Load(); err != nil {
			return err
		}
	}
	active, total := cfg.ConnectionLimits()
	shownProxy := proxydial.Redact(cfg.ConnectionsProxy)
	if jsonOut {
		// Per provider and profile: the proxy in force, whether the object sets it
		// itself, and (profiles) the level it comes from.
		providers := map[string]any{}
		for name, pr := range cfg.Providers {
			providers[name] = map[string]any{"proxy": cfg.ProviderProxy(name).String(), "own": pr.Proxy != ""}
		}
		profiles := map[string]any{}
		for name, pf := range cfg.Profiles {
			profiles[name] = map[string]any{"proxy": cfg.ProxyChoice(name).String(), "own": pf.Proxy != "", "level": cfg.ProxyLevel(name)}
		}
		return writeJSON(w, map[string]any{
			"active": active, "total": total,
			"active_default": cfg.ConnectionsActive == 0, "total_default": cfg.ConnectionsTotal == 0,
			"proxy": shownProxy, "providers": providers, "profiles": profiles,
		})
	}
	def := func(v int64) string {
		if v == 0 {
			return " (default)"
		}
		return ""
	}
	_, _ = fmt.Fprintf(w, "active: %-3d connections per backend that take new requests%s\n", active, def(cfg.ConnectionsActive))
	_, _ = fmt.Fprintf(w, "total:  %-3d active plus ones still finishing older requests%s\n", total, def(cfg.ConnectionsTotal))
	if shownProxy == "" {
		_, _ = fmt.Fprintln(w, "proxy:  none (direct; HTTPS_PROXY / HTTP_PROXY from the environment still apply)")
	} else {
		_, _ = fmt.Fprintf(w, "proxy:  %s\n", shownProxy)
	}
	for _, name := range sortedKeys(cfg.Providers) {
		if cfg.Providers[name].Proxy != "" {
			_, _ = fmt.Fprintf(w, "provider %s: proxy %s (its own; replaces the global one for its profiles)\n", name, cfg.ProviderProxy(name))
		}
	}
	for _, name := range sortedKeys(cfg.Profiles) {
		if pf := cfg.Profiles[name]; pf.Proxy != "" {
			_, _ = fmt.Fprintf(w, "profile %s: proxy %s (its own; replaces the provider's and the global one)\n", name, cfg.ProxyChoice(name))
		}
	}
	if len(set) > 0 || setProxy {
		if backup != "" {
			_, _ = fmt.Fprintf(w, "config.ktav updated (old one saved as %s); the running server applies it on its next request\n", backup)
		} else {
			_, _ = fmt.Fprintln(w, "config.ktav already had these values")
		}
	}
	return nil
}

// providerProxyValue is proxyValue for "proxy VALUE provider NAME": "none" /
// "off" writes none (direct, the global proxy is not inherited), "inherit" /
// "default" removes the override.
func providerProxyValue(cfg *config.Config, value, provider string) (string, error) {
	pr, ok := cfg.Providers[provider]
	if !ok {
		return "", fmt.Errorf("unknown provider %q (configured: %s)", provider, names(cfg.Providers))
	}
	if pr.Type == config.ProviderTypeClaude {
		return "", fmt.Errorf("provider %q is a Claude profile: it does not go through ccgp, so it has no proxy setting", provider)
	}
	switch value {
	case "inherit", "default":
		return "", nil
	case "off", "none":
		return config.ProxyNone, nil
	}
	_, err := proxydial.Parse(value)
	return value, err
}

// proxyValue turns the words of "proxy VALUE [profile NAME]" into the text for
// the config: "" removes the line. Globally "off" / "none" removes it; for a
// profile "none" / "off" writes none (direct, nothing inherited) and "inherit" /
// "default" removes the override.
func proxyValue(cfg *config.Config, value, profile string) (string, error) {
	off := value == "off" || value == "none"
	if profile == "" {
		if off {
			return "", nil
		}
		_, err := proxydial.Parse(value)
		return value, err
	}
	pf, ok := cfg.Profiles[profile]
	if !ok {
		return "", fmt.Errorf("unknown profile %q (configured: %s)", profile, names(cfg.Profiles))
	}
	if isClaudeProfile(cfg, pf) {
		return "", fmt.Errorf("profile %q is a Claude profile: it does not go through ccgp, so it has no proxy setting", profile)
	}
	switch {
	case value == "inherit" || value == "default":
		return "", nil
	case off:
		return config.ProxyNone, nil
	}
	_, err := proxydial.Parse(value)
	return value, err
}
