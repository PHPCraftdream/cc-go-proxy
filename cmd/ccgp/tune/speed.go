package tune

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgedit"
)

const speedUsage = "usage: ccgp --speed [<profile> <family> [fast|standard|unset|inherit]]"

var SpeedFamilies = []string{"astra", "luna", "sol", "terra"}

func SpeedCmd(args []string) error { return Speed(os.Stdout, args) }

func SpeedPolicy(name, family, value string) string {
	if value == "" {
		return fmt.Sprintf("%s %s: inherit (client request)", name, family)
	}
	return fmt.Sprintf("%s %s: %s (profile rule)", name, family, value)
}

func ListSpeed(w io.Writer, cfg *config.Config) {
	found := false
	for _, name := range common.SortedKeys(cfg.Profiles) {
		p := cfg.Profiles[name]
		for _, family := range common.SortedKeys(p.SpeedRules) {
			_, _ = fmt.Fprintln(w, SpeedPolicy(name, family, p.SpeedFor(family)))
			found = true
		}
	}
	if !found {
		_, _ = fmt.Fprintln(w, "no explicit speed rules")
	}
}

func Speed(w io.Writer, args []string) error {
	if len(args) != 0 && len(args) != 2 && len(args) != 3 {
		return errors.New(speedUsage)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		ListSpeed(w, cfg)
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
		backup, err := cfgedit.EditFile(cfg.ConfigFile, func(text string) (string, error) { return cfgedit.SetProfileSpeedText(text, name, family, value) })
		if err != nil {
			return err
		}
		if backup != "" {
			_, _ = fmt.Fprintf(w, "config.ktav updated (old one saved as %s)\n", backup)
		} else {
			_, _ = fmt.Fprintln(w, "config.ktav unchanged")
		}
	}
	_, _ = fmt.Fprintln(w, SpeedPolicy(name, family, value))
	return nil
}
