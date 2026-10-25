package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"golang.org/x/term"
)

func logoutCmd(args []string) error {
	yes, rest := false, []string{}
	for _, a := range args {
		if a == "--yes" {
			yes = true
		} else {
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		return errors.New("usage: ccgp --logout <provider> [--yes]")
	}
	if !yes && !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("refusing to log out without a terminal; pass --yes to confirm")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := credsStore()
	if err != nil {
		return err
	}
	return logout(os.Stdout, os.Stdin, store, cfg, rest[0], yes)
}

// logout is the testable core of `ccgp --logout`. It removes the provider's
// stored credentials only; config.ktav is never written. Any stored file
// counts, whatever the provider's current type or api_key_env: a corrupt or
// stale file must not be left behind.
func logout(w io.Writer, in io.Reader, store creds.Store, cfg *config.Config, provider string, yes bool) error {
	if cfg.IsEnvMode() {
		return errors.New("ccgp --logout needs config.ktav (env-only mode has no stored logins)")
	}
	pr, ok := cfg.Providers[provider]
	if !ok {
		return fmt.Errorf("unknown provider %q (configured: %s)", provider, names(cfg.Providers))
	}
	if !store.HasProviderCreds(provider) {
		if pr.APIKeyEnv != "" {
			_, _ = fmt.Fprintf(w, "provider %q reads its key from $%s, there is no stored login (the environment variable is not touched)\n", provider, pr.APIKeyEnv)
		} else {
			_, _ = fmt.Fprintf(w, "provider %q has no stored login\n", provider)
		}
		return nil
	}
	if !yes {
		_, _ = fmt.Fprintf(w, "remove the login of provider %q? [y/N] ", provider)
		line, _ := bufio.NewReader(in).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			_, _ = fmt.Fprintln(w, "aborted")
			return nil
		}
	}
	if err := store.DeleteProviderCreds(provider); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "logged out of %s\n", provider)
	_, _ = fmt.Fprintf(w, "login again with: ccgp --login %s\n", provider)
	return nil
}
