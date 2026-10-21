package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"golang.org/x/term"
)

func credsStore() (creds.Store, error) {
	dir, err := creds.DefaultDir()
	return creds.Store{Dir: dir}, err
}

// login authenticates a provider. force=false skips when credentials exist.
func login(cfg *config.Config, provider string, force, device bool) error {
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
		fresh, err := loginCodex(store, pr.Name, force, device)
		if err == nil && fresh {
			autoApplyModels(cfg, pr.Name)
		}
		return err
	case config.ProviderTypeCodexAPI:
		_, err := loginAPIKey(store, pr, force)
		return err
	case config.ProviderTypeAnthropicCompat:
		fresh, err := loginAPIKey(store, pr, force)
		if err == nil && fresh {
			autoApplyPreset(cfg, pr.Name)
		}
		return err
	}
	return fmt.Errorf("provider %q: unsupported type %q", pr.Name, pr.Type)
}

// loginCodex signs in with ChatGPT OAuth: browser callback flow by default,
// device-code flow with device=true. The browser is never opened automatically.
func loginCodex(store creds.Store, provider string, force, device bool) (fresh bool, err error) {
	ts := oauth.Store{Path: store.OAuthPath(provider)}
	if !force {
		if _, lerr := ts.Load(); lerr == nil {
			return false, nil
		} else if !errors.Is(lerr, oauth.ErrNotLoggedIn) {
			return false, lerr
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var tok *oauth.Tokens
	if device {
		tok, err = deviceLogin(ctx, provider)
	} else {
		tok, err = browserLogin(ctx, provider)
	}
	if err != nil {
		return false, err
	}
	if err := ts.Save(tok); err != nil {
		return false, err
	}
	fmt.Fprintf(os.Stderr, "Logged in (provider %q).\n", provider)
	return true, nil
}

func browserLogin(ctx context.Context, provider string) (*oauth.Tokens, error) {
	tok, err := (&oauth.Client{}).BrowserLogin(ctx, func(authURL string) {
		fmt.Fprintf(os.Stderr, "\nSign in to ChatGPT for provider %q.\n\n", provider)
		fmt.Fprintf(os.Stderr, "Open this link in a browser on this computer:\n\n  %s\n\n", authURL)
		fmt.Fprintf(os.Stderr, "After you sign in, the browser returns to localhost:%s and ccgp continues.\n", oauth.CallbackPort)
		fmt.Fprintf(os.Stderr, "No browser on this computer? Use: ccgp login %s --device\n\nWaiting (10 minutes)...\n", provider)
	})
	if err != nil {
		return nil, err
	}
	return tok, nil
}

func deviceLogin(ctx context.Context, provider string) (*oauth.Tokens, error) {
	c := &oauth.Client{}
	dc, err := c.RequestDeviceCode(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "\nSign in to ChatGPT for provider %q (device code):\n\n", provider)
	fmt.Fprintf(os.Stderr, "  1. Open in a browser:  %s\n", dc.VerificationURL)
	fmt.Fprintf(os.Stderr, "  2. Enter the code:     %s   (expires in 15 minutes)\n\n", dc.UserCode)
	fmt.Fprintln(os.Stderr, "Device-code sign-in must be enabled in ChatGPT Security Settings.")
	fmt.Fprintln(os.Stderr, "Only continue if you started this login yourself. Waiting...")
	return c.CompleteDeviceLogin(ctx, dc)
}

func loginAPIKey(store creds.Store, pr *config.Provider, force bool) (fresh bool, err error) {
	if pr.APIKeyEnv != "" {
		if os.Getenv(pr.APIKeyEnv) == "" {
			return false, fmt.Errorf("provider %q reads its key from $%s, which is empty", pr.Name, pr.APIKeyEnv)
		}
		return false, nil
	}
	if !force {
		if k, err := store.APIKey(pr.Name); err != nil || k != "" {
			return false, err
		}
		if isLocal(pr.BaseURL) {
			return false, nil // local backends (Ollama) need no key
		}
	}
	fmt.Fprintf(os.Stderr, "API key for provider %q (%s): ", pr.Name, pr.BaseURL)
	key, err := readSecret()
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return false, err
	}
	if key == "" {
		return false, errors.New("empty API key")
	}
	if err := store.SaveAPIKey(pr.Name, key); err != nil {
		return false, err
	}
	fmt.Fprintf(os.Stderr, "API key saved (provider %q).\n", pr.Name)
	return true, nil
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

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func names[T any](m map[string]T) string {
	if len(m) == 0 {
		return "none"
	}
	return strings.Join(sortedKeys(m), ", ")
}

// loginState describes stored credentials without exposing them.
func loginState(store creds.Store, pr *config.Provider) string {
	switch pr.Type {
	case config.ProviderTypeCodex:
		t, err := oauth.Store{Path: store.OAuthPath(pr.Name)}.Load()
		switch {
		case errors.Is(err, oauth.ErrNotLoggedIn):
			return "not logged in (ccgp login " + pr.Name + ")"
		case err != nil:
			return "error: " + err.Error()
		}
		return "logged in, tokens refreshed " + t.LastRefresh.Local().Format("2006-01-02 15:04")
	case config.ProviderTypeCodexAPI, config.ProviderTypeAnthropicCompat:
		if pr.APIKeyEnv != "" {
			if os.Getenv(pr.APIKeyEnv) == "" {
				return "$" + pr.APIKeyEnv + " is empty"
			}
			return "key from $" + pr.APIKeyEnv
		}
		k, err := store.APIKey(pr.Name)
		switch {
		case err != nil:
			return "error: " + err.Error()
		case k != "":
			return "API key saved"
		case isLocal(pr.BaseURL):
			return "no key needed (local)"
		}
		return "no API key (ccgp login " + pr.Name + ")"
	}
	return "unknown type"
}
