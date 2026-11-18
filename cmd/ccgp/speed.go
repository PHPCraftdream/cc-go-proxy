package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

const speedUsage = "usage: ccgp --speed [<profile> <family> [fast|standard|unset|inherit]]"

var speedFamilies = []string{"astra", "luna", "sol", "terra"}

func speedCmd(args []string) error { return speed(os.Stdout, args) }

func speedPolicy(name, family, value string) string {
	if value == "" {
		return fmt.Sprintf("%s %s: inherit (client request)", name, family)
	}
	return fmt.Sprintf("%s %s: %s (profile rule)", name, family, value)
}

func listSpeed(w io.Writer, cfg *config.Config) {
	found := false
	for _, name := range sortedKeys(cfg.Profiles) {
		p := cfg.Profiles[name]
		for _, family := range sortedKeys(p.SpeedRules) {
			_, _ = fmt.Fprintln(w, speedPolicy(name, family, p.SpeedFor(family)))
			found = true
		}
	}
	if !found {
		_, _ = fmt.Fprintln(w, "no explicit speed rules")
	}
}

func speed(w io.Writer, args []string) error {
	if len(args) != 0 && len(args) != 2 && len(args) != 3 {
		return errors.New(speedUsage)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		listSpeed(w, cfg)
		return nil
	}
	name, family := args[0], args[1]
	p, ok := cfg.Profiles[name]
	if !ok || p == nil {
		return fmt.Errorf("unknown profile %q", name)
	}
	provider := cfg.Providers[p.Provider]
	if provider == nil || provider.Type != config.ProviderTypeCodex {
		return fmt.Errorf("profile %q is not a Codex profile", name)
	}
	if family == "" {
		return errors.New("family must contain lowercase letters only")
	}
	for _, c := range family {
		if c < 'a' || c > 'z' {
			return errors.New("family must contain lowercase letters only")
		}
	}
	value := p.SpeedFor(family)
	if len(args) == 3 {
		value = args[2]
		switch value {
		case "unset", "inherit":
			value = ""
		case config.SpeedFast, config.SpeedStandard:
		default:
			return errors.New(speedUsage)
		}
		if cfg.IsEnvMode() {
			return errors.New("no config.ktav (env-only mode): nothing to edit")
		}
		backup, err := config.EditFile(cfg.ConfigFile, func(text string) (string, error) { return config.SetProfileSpeedText(text, name, family, value) })
		if err != nil {
			return err
		}
		if backup != "" {
			_, _ = fmt.Fprintf(w, "config.ktav updated (old one saved as %s)\n", backup)
		} else {
			_, _ = fmt.Fprintln(w, "config.ktav unchanged")
		}
	}
	_, _ = fmt.Fprintln(w, speedPolicy(name, family, value))
	return nil
}
