package main

import "testing"

// The proxy of a provider is edited on its screen, and a profile says which
// level its own proxy comes from.
func TestTUIProviderProxy(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("2", "3", "3") // Providers, zai, proxy
	if got := string(h.input().text); got != "" {
		t.Errorf("the field starts with %q", got)
	}
	h.typ("http://corp:3128")
	h.press("enter")
	h.has("proxy http://corp:3128 (its own)", "3 proxy http://corp:3128", "config.ktav updated")
	if pr := h.config().Providers["zai"]; pr.Proxy != "http://corp:3128" {
		t.Errorf("provider proxy = %q", pr.Proxy)
	}

	// The profile of the provider goes through it, until it has a proxy of its own.
	h.press("esc", "esc", "1", "3") // main menu, Profiles, zai
	h.has("proxy http://corp:3128 (of provider zai)")
	h.press("4")
	h.typ("socks5://own:1080")
	h.press("enter")
	h.has("proxy socks5://own:1080 (its own)")
	h.press("4")
	h.replace("none")
	h.press("enter")
	h.has("proxy none (its own)")
	h.press("4", "ctrl+u", "enter") // empty: the provider's proxy again
	h.has("proxy http://corp:3128 (of provider zai)")

	// none for the provider: direct; empty: the global proxy again.
	h.press("esc", "esc", "2", "3", "3")
	h.replace("none")
	h.press("enter")
	h.has("proxy none (its own)")
	h.press("3", "ctrl+u", "enter")
	h.has("proxy global (")
	if pr := h.config().Providers["zai"]; pr.Proxy != "" {
		t.Errorf("provider proxy after emptying = %q", pr.Proxy)
	}

	// A wrong proxy changes nothing.
	before := h.text()
	h.press("3")
	h.typ("ftp://nope")
	h.press("enter")
	h.has("error:")
	h.press("esc")
	if h.text() != before {
		t.Error("a wrong proxy changed the config")
	}
}

// codex has no base_url to show but a proxy and peak hours all the same; a
// Claude provider has no proxy at all.
func TestTUIProviderProxyOfOtherKinds(t *testing.T) {
	h := newTUI(t, uiConfig)
	h.press("2", "1", "2") // Providers, codex, proxy
	h.typ("socks5://127.0.0.1:1080")
	h.press("enter")
	h.has("Provider codex", "proxy socks5://127.0.0.1:1080 (its own)")
	if pr := h.config().Providers["codex"]; pr.Proxy != "socks5://127.0.0.1:1080" {
		t.Errorf("codex proxy = %q", pr.Proxy)
	}

	h = newTUI(t, uiConfig)
	h.press("2", "2")
	h.has("Provider work", "type claude")
	h.lacks("proxy")
}
