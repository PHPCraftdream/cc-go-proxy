package cfgedit

import (
	"errors"
	"fmt"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgtext"
)

// Scalar keys of an object that `ccgp --interactive` may set. The proxy of a
// profile and of a provider have their own editors (SetProfileProxyText,
// SetProviderProxyText), the peak hours too (SetProviderPeakText); models and
// effort_map are lists and stay hand work.
var (
	profileKeys  = []string{"default_model", "context_window", "compact_percent", "provider"}
	providerKeys = []string{"base_url", "api_key_env"}
)

// ProfileKeys and ProviderKeys are the scalar keys SetProfileKeyText and
// SetProviderKeyText take.
func ProfileKeys() []string { return append([]string(nil), profileKeys...) }

func ProviderKeys() []string { return append([]string(nil), providerKeys...) }

// SetProfileKeyText sets default_model, context_window, compact_percent or
// provider in a profile's object in config text; an empty value removes the line
// (a profile always keeps its provider). A new line goes after the provider line.
func SetProfileKeyText(text, profile, key, value string) (string, error) {
	if !cfgtext.InList(profileKeys, key) {
		return "", fmt.Errorf("profile key %q cannot be set here (%s)", key, strings.Join(profileKeys, ", "))
	}
	if key == "provider" && strings.TrimSpace(value) == "" {
		return "", errors.New("a profile needs a provider")
	}
	lines := strings.Split(text, "\n")
	open, closeAt, _, err := cfgtext.FindProfileObject(lines, profile)
	if err != nil {
		return "", err
	}
	return cfgtext.SetScalarKey(lines, open, closeAt, key, value, []string{"provider", "name"})
}

// SetProviderKeyText sets base_url or api_key_env in a provider's object in
// config text; an empty value removes the line. A new line goes after the
// provider's name, type, base_url and api_key_env lines.
func SetProviderKeyText(text, provider, key, value string) (string, error) {
	if !cfgtext.InList(providerKeys, key) {
		return "", fmt.Errorf("provider key %q cannot be set here (%s)", key, strings.Join(providerKeys, ", "))
	}
	return setProviderKey(text, provider, key, value)
}

// SetProviderProxyText sets proxy in the provider's object in config text: a
// proxy URL, config.ProxyNone, or "" to remove the line (the provider inherits the
// global proxy again). The line goes after the provider's name, type, base_url
// and api_key_env lines.
func SetProviderProxyText(text, provider, proxy string) (string, error) {
	if err := config.CheckProviderProxy(provider, proxy); err != nil {
		return "", err
	}
	return setProviderKey(text, provider, "proxy", proxy)
}

func setProviderKey(text, provider, key, value string) (string, error) {
	lines := strings.Split(text, "\n")
	prs, err := cfgtext.FindList(lines, "providers")
	if err != nil {
		return "", err
	}
	open, closeAt, ok := cfgtext.FindObjectNamed(lines, prs.Open, prs.Close, provider, false)
	if !ok {
		return "", fmt.Errorf("provider %q not found in the config", provider)
	}
	return cfgtext.SetScalarKey(lines, open, closeAt, key, value, []string{"name", "type", "base_url", "api_key_env"})
}
