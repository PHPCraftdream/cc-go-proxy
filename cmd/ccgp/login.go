package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/creds"
	"github.com/claude-code-proxy/proxy/internal/oauth"
	"golang.org/x/term"
)

func credsStore() (creds.Store, error) {
	dir, err := creds.DefaultDir()
	return creds.Store{Dir: dir}, err
}

// login authenticates a provider. force=false skips when credentials exist.
func login(cfg *config.Config, provider string, force bool) error {
	if cfg.IsEnvMode() {
		return errors.New("login needs config.ktav; env-only mode uses OPENAI_API_KEY")
	}
	pr, ok := cfg.Providers[provider]
	if !ok {
		return fmt.Errorf("unknown provider %q (configured: %s)", provider, names(cfg.Providers))
	}
	store, err := credsStore()
	if err != nil {
		return err
	}
	switch pr.Type {
	case config.ProviderTypeCodex:
		return loginCodex(store, pr.Name, force)
	case config.ProviderTypeOpenAI:
		return loginAPIKey(store, pr, force)
	}
	return fmt.Errorf("provider %q: unsupported type %q", pr.Name, pr.Type)
}

func loginCodex(store creds.Store, provider string, force bool) error {
	ts := oauth.Store{Path: store.OAuthPath(provider)}
	if !force {
		if _, err := ts.Load(); err == nil {
			return nil
		} else if !errors.Is(err, oauth.ErrNotLoggedIn) {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	c := &oauth.Client{}
	dc, err := c.RequestDeviceCode(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nSign in to ChatGPT for provider %q:\n\n", provider)
	fmt.Fprintf(os.Stderr, "  1. Open in a browser:  %s\n", dc.VerificationURL)
	fmt.Fprintf(os.Stderr, "  2. Enter the code:     %s   (expires in 15 minutes)\n\n", dc.UserCode)
	fmt.Fprintln(os.Stderr, "Only continue if you started this login yourself. Waiting...")

	tok, err := c.CompleteDeviceLogin(ctx, dc)
	if err != nil {
		return err
	}
	if err := ts.Save(tok); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Logged in (provider %q).\n", provider)
	return nil
}

func loginAPIKey(store creds.Store, pr *config.Provider, force bool) error {
	if pr.APIKeyEnv != "" {
		if os.Getenv(pr.APIKeyEnv) == "" {
			return fmt.Errorf("provider %q reads its key from $%s, which is empty", pr.Name, pr.APIKeyEnv)
		}
		return nil
	}
	if !force {
		if k, err := store.APIKey(pr.Name); err != nil || k != "" {
			return err
		}
		if isLocal(pr.BaseURL) {
			return nil // local backends (Ollama) need no key
		}
	}
	fmt.Fprintf(os.Stderr, "API key for provider %q (%s): ", pr.Name, pr.BaseURL)
	key, err := readSecret()
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	if key == "" {
		return errors.New("empty API key")
	}
	if err := store.SaveAPIKey(pr.Name, key); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "API key saved (provider %q).\n", pr.Name)
	return nil
}

// readSecret reads a line without echo when stdin is a terminal.
func readSecret() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func isLocal(u string) bool {
	return (&config.Config{OpenAIBaseURL: u}).IsLocalhost()
}

func names[T any](m map[string]T) string {
	if len(m) == 0 {
		return "none"
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return strings.Join(out, ", ")
}
