package config

import (
	"errors"
	"fmt"
	"strings"
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
func ProfileKeys() []string  { return append([]string(nil), profileKeys...) }
func ProviderKeys() []string { return append([]string(nil), providerKeys...) }

func inList(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// SetProfileKeyText sets default_model, context_window, compact_percent or
// provider in a profile's object in config text; an empty value removes the line
// (a profile always keeps its provider). A new line goes after the provider line.
func SetProfileKeyText(text, profile, key, value string) (string, error) {
	if !inList(profileKeys, key) {
		return "", fmt.Errorf("profile key %q cannot be set here (%s)", key, strings.Join(profileKeys, ", "))
	}
	if key == "provider" && strings.TrimSpace(value) == "" {
		return "", errors.New("a profile needs a provider")
	}
	lines := strings.Split(text, "\n")
	open, closeAt, _, err := findProfileObject(lines, profile)
	if err != nil {
		return "", err
	}
	return setScalarKey(lines, open, closeAt, key, value, []string{"provider", "name"})
}

// SetProviderKeyText sets base_url or api_key_env in a provider's object in
// config text; an empty value removes the line. A new line goes after the
// provider's name, type, base_url and api_key_env lines.
func SetProviderKeyText(text, provider, key, value string) (string, error) {
	if !inList(providerKeys, key) {
		return "", fmt.Errorf("provider key %q cannot be set here (%s)", key, strings.Join(providerKeys, ", "))
	}
	return setProviderKey(text, provider, key, value)
}

// SetProviderProxyText sets proxy in the provider's object in config text: a
// proxy URL, ProxyNone, or "" to remove the line (the provider inherits the
// global proxy again). The line goes after the provider's name, type, base_url
// and api_key_env lines.
func SetProviderProxyText(text, provider, proxy string) (string, error) {
	if err := validateProviderProxy(provider, proxy); err != nil {
		return "", err
	}
	return setProviderKey(text, provider, "proxy", proxy)
}

func setProviderKey(text, provider, key, value string) (string, error) {
	lines := strings.Split(text, "\n")
	prs, err := findList(lines, "providers")
	if err != nil {
		return "", err
	}
	open, closeAt, ok := findObjectNamed(lines, prs.open, prs.close, provider, false)
	if !ok {
		return "", fmt.Errorf("provider %q not found in the config", provider)
	}
	return setScalarKey(lines, open, closeAt, key, value, []string{"name", "type", "base_url", "api_key_env"})
}

// setScalarKey replaces, removes or adds the line `key: value` at the object's
// own indent (nested objects are skipped). A new line goes after the last line
// whose key is in after, else right after the opening brace.
func setScalarKey(lines []string, open, closeAt int, key, value string, after []string) (string, error) {
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%s: the value must be one line", key)
	}
	value = strings.TrimSpace(value)
	child := indentOf(lines[open]) + "    "
	for i := open + 1; i < closeAt; i++ {
		if _, _, ok := keyValue(lines[i]); ok {
			child = indentOf(lines[i])
			break
		}
	}
	keyAt, afterAt := -1, open
	for i := open + 1; i < closeAt; i++ {
		if indentOf(lines[i]) != child {
			continue
		}
		k, _, ok := keyValue(lines[i])
		if !ok {
			continue
		}
		if k == key {
			keyAt = i
		}
		if inList(after, k) {
			afterAt = i
		}
	}
	switch {
	case keyAt >= 0 && value == "":
		return strings.Join(append(lines[:keyAt:keyAt], lines[keyAt+1:]...), "\n"), nil
	case keyAt >= 0:
		cr := lines[keyAt][len(strings.TrimRight(lines[keyAt], "\r")):]
		lines[keyAt] = child + key + ": " + value + cr
		return strings.Join(lines, "\n"), nil
	case value == "":
		return strings.Join(lines, "\n"), nil
	}
	add := withEndings([]string{child + key + ": " + value}, dominantCRLF(lines))
	out := append(append(append([]string{}, lines[:afterAt+1]...), add...), lines[afterAt+1:]...)
	return strings.Join(out, "\n"), nil
}
