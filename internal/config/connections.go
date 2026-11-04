package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

// Upstream connections per backend: connections.active take new requests,
// connections.total caps them together with the ones still finishing older
// requests (see internal/connpool); connections.proxy, when set, carries them.
const (
	DefaultConnectionsActive = connpool.DefaultActive
	DefaultConnectionsTotal  = connpool.DefaultTotal
	maxConnections           = 64
)

func validateConnections(active, total int64, proxy string) error {
	if active < 0 || active > maxConnections {
		return fmt.Errorf("connections.active must be 0-%d (0: default %d)", maxConnections, DefaultConnectionsActive)
	}
	if total < 0 || total > maxConnections {
		return fmt.Errorf("connections.total must be 0-%d (0: default %d)", maxConnections, DefaultConnectionsTotal)
	}
	if a, t := effectiveConnections(active, total); t < a {
		return fmt.Errorf("connections.total (%d) must not be below connections.active (%d)", t, a)
	}
	if proxy != "" {
		if _, err := proxydial.Parse(proxy); err != nil {
			return fmt.Errorf("connections.proxy: %w", err)
		}
	}
	return nil
}

func effectiveConnections(active, total int64) (int, int) {
	if active == 0 {
		active = DefaultConnectionsActive
	}
	if total == 0 {
		total = DefaultConnectionsTotal
	}
	return int(active), int(total)
}

// ProxyNone, as a provider's or profile's proxy, means "no proxy" (nothing is
// inherited from the levels above).
const ProxyNone = "none"

func validateProfileProxy(profile, proxy string) error {
	return validateScopeProxy("profile", profile, proxy)
}

func validateProviderProxy(provider, proxy string) error {
	return validateScopeProxy("provider", provider, proxy)
}

func validateScopeProxy(kind, name, proxy string) error {
	if proxy == "" || proxy == ProxyNone {
		return nil
	}
	if _, err := proxydial.Parse(proxy); err != nil {
		return fmt.Errorf("%s %q: proxy: %w (a proxy URL, or %s)", kind, name, err, ProxyNone)
	}
	return nil
}

// ConnectionLimits returns the effective connections.active and .total.
func (c *Config) ConnectionLimits() (active, total int) {
	return effectiveConnections(c.ConnectionsActive, c.ConnectionsTotal)
}

// ProxyURL returns connections.proxy, nil when unset (direct, or the
// HTTPS_PROXY / HTTP_PROXY environment). The loader has validated it.
func (c *Config) ProxyURL() *url.URL {
	if c.ConnectionsProxy == "" {
		return nil
	}
	u, err := proxydial.Parse(c.ConnectionsProxy)
	if err != nil {
		return nil
	}
	return u
}

// proxySetting reads the proxy value of one level (a profile or a provider):
// ok is false when the level sets none and the level above decides. A level
// that sets one replaces the upper ones whole, it is never merged with them.
func proxySetting(v string) (ch proxydial.Choice, ok bool) {
	switch v {
	case "":
		return ch, false
	case ProxyNone:
		return proxydial.Choice{Direct: true}, true
	}
	u, err := proxydial.Parse(v)
	if err != nil {
		return ch, false
	}
	return proxydial.Choice{URL: u}, true
}

// ProviderProxy is the way out of a provider's profiles that set none: the
// provider's own proxy, or "none", else the global connections.proxy, else the
// environment's.
func (c *Config) ProviderProxy(provider string) proxydial.Choice {
	if pr := c.Providers[provider]; pr != nil {
		if ch, ok := proxySetting(pr.Proxy); ok {
			return ch
		}
	}
	return proxydial.Choice{URL: c.ProxyURL()}
}

// ProxyChoice is the way out of a profile's requests, from the nearest level
// that sets one: the profile's proxy, the provider's, the global
// connections.proxy, the environment's. "none" at a level means direct and
// nothing from the levels above applies.
func (c *Config) ProxyChoice(profile string) proxydial.Choice {
	pf := c.Profiles[profile]
	if pf == nil {
		return proxydial.Choice{URL: c.ProxyURL()}
	}
	if ch, ok := proxySetting(pf.Proxy); ok {
		return ch
	}
	return c.ProviderProxy(pf.Provider)
}

// Levels of the proxy cascade, as ProxyLevel names them.
const (
	ProxyLevelProfile  = "profile"
	ProxyLevelProvider = "provider"
	ProxyLevelGlobal   = "global"
)

// ProxyLevel names the level that decides a profile's proxy: ProxyLevelProfile,
// ProxyLevelProvider, ProxyLevelGlobal, or "" when none sets one (the
// environment's proxy applies, if any).
func (c *Config) ProxyLevel(profile string) string {
	if pf := c.Profiles[profile]; pf != nil {
		if _, ok := proxySetting(pf.Proxy); ok {
			return ProxyLevelProfile
		}
		if pr := c.Providers[pf.Provider]; pr != nil {
			if _, ok := proxySetting(pr.Proxy); ok {
				return ProxyLevelProvider
			}
		}
	}
	if c.ProxyURL() != nil {
		return ProxyLevelGlobal
	}
	return ""
}

// ProviderProxyChoice is the way out for what belongs to a provider rather
// than a request (login, the model catalog): the proxy its profiles agree on,
// the provider's own (else the global one) when they differ or there are none.
func (c *Config) ProviderProxyChoice(provider string) proxydial.Choice {
	var common proxydial.Choice
	found := false
	for name, pf := range c.Profiles {
		if pf.Provider != provider {
			continue
		}
		ch := c.ProxyChoice(name)
		if found && ch.Key() != common.Key() {
			return c.ProviderProxy(provider)
		}
		common, found = ch, true
	}
	if !found {
		return c.ProviderProxy(provider)
	}
	return common
}

// SetConnectionsText sets connections.<key> (active or total) in config text:
// the `connections.<key>:` line is rewritten, or added after the listen and
// connections settings (at the top when there are none). A `connections: {` block must be
// edited by hand.
func SetConnectionsText(text, key string, value int64) (string, error) {
	if key != "active" && key != "total" {
		return "", fmt.Errorf("unknown connections setting %q (active or total)", key)
	}
	return setConnectionsLine(text, key, strconv.FormatInt(value, 10))
}

// SetConnectionsProxyText sets connections.proxy in config text like
// SetConnectionsText; an empty proxy removes the line.
func SetConnectionsProxyText(text, proxy string) (string, error) {
	if proxy == "" {
		lines := strings.Split(text, "\n")
		out := lines[:0]
		for _, l := range lines {
			if !proxyLine.MatchString(strings.TrimRight(l, "\r")) {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n"), nil
	}
	if _, err := proxydial.Parse(proxy); err != nil {
		return "", err
	}
	return setConnectionsLine(text, "proxy", proxy)
}

func setConnectionsLine(text, key, value string) (string, error) {
	lines := strings.Split(text, "\n")
	line := "connections." + key + ": " + value
	re := regexp.MustCompile(`^connections\.` + key + `\s*:`)
	lastListen, block := -1, false
	for i, l := range lines {
		trimmed := strings.TrimRight(l, "\r")
		switch {
		case re.MatchString(trimmed):
			lines[i] = line + l[len(trimmed):]
			return strings.Join(lines, "\n"), nil
		case strings.HasPrefix(trimmed, "listen."), strings.HasPrefix(trimmed, "connections."):
			lastListen = i
		case connectionsBlock.MatchString(trimmed):
			block = true
		}
	}
	if block {
		return "", fmt.Errorf("config has a `connections:` block; set %s there by hand", key)
	}
	if strings.HasSuffix(lines[0], "\r") {
		line += "\r"
	}
	at := lastListen + 1
	if lastListen < 0 {
		// After the leading comment lines.
		for at < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[at]), "#") {
			at++
		}
	}
	out := append([]string{}, lines[:at]...)
	out = append(out, line)
	return strings.Join(append(out, lines[at:]...), "\n"), nil
}

var (
	connectionsBlock = regexp.MustCompile(`^connections\s*:`)
	proxyLine        = regexp.MustCompile(`^connections\.proxy\s*:`)
)

// SetProfileProxyText sets proxy in the profile's object in config text: a
// proxy URL, ProxyNone, or "" to remove the line (the profile inherits the
// global proxy again). The line goes after the profile's provider line.
func SetProfileProxyText(text, profile, proxy string) (string, error) {
	if proxy != "" && proxy != ProxyNone {
		if _, err := proxydial.Parse(proxy); err != nil {
			return "", err
		}
	}
	lines := strings.Split(text, "\n")
	open, closeAt, _, err := findProfileObject(lines, profile)
	if err != nil {
		return "", err
	}
	child := indentOf(lines[open]) + "    "
	if open+1 < closeAt {
		child = indentOf(lines[open+1])
	}
	proxyAt, providerAt := -1, -1
	for i := open + 1; i < closeAt; i++ {
		if indentOf(lines[i]) != child {
			continue // nested (models rules)
		}
		switch k, _, ok := keyValue(lines[i]); {
		case ok && k == "proxy":
			proxyAt = i
		case ok && k == "provider":
			providerAt = i
		}
	}
	switch {
	case proxyAt >= 0 && proxy == "":
		return strings.Join(append(lines[:proxyAt:proxyAt], lines[proxyAt+1:]...), "\n"), nil
	case proxyAt >= 0:
		cr := lines[proxyAt][len(strings.TrimRight(lines[proxyAt], "\r")):]
		lines[proxyAt] = child + "proxy: " + proxy + cr
		return strings.Join(lines, "\n"), nil
	case proxy == "":
		return text, nil
	}
	if providerAt < 0 {
		providerAt = open
	}
	add := withEndings([]string{child + "proxy: " + proxy}, dominantCRLF(lines))
	return strings.Join(append(append(lines[:providerAt+1:providerAt+1], add...), lines[providerAt+1:]...), "\n"), nil
}
