package codex

import (
	"strings"
	"testing"
)

func TestParseCatalog(t *testing.T) {
	for name, body := range map[string]string{
		"models": `{"models":[{"slug":"gpt-6-sol","display_name":"Sol","context_window":1050000},{"slug":"gpt-5.6-luna","visibility":"hide","context_window":372000}]}`,
		"data":   `{"data":[{"id":"gpt-6-sol","display_name":"Sol","context_window":1050000},{"id":"gpt-5.6-luna","visibility":"hidden","context_window":372000}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			ms, err := ParseCatalog([]byte(body))
			if err != nil || len(ms) != 2 {
				t.Fatalf("ParseCatalog = %v, %v", ms, err)
			}
			if ms[0].Slug != "gpt-6-sol" || ms[0].ContextWindow != 1050000 || ms[0].Hidden {
				t.Errorf("first = %+v", ms[0])
			}
			if !ms[1].Hidden || ms[1].ContextWindow != 372000 {
				t.Errorf("second = %+v", ms[1])
			}
		})
	}
	if _, err := ParseCatalog([]byte(`{"other":1}`)); err == nil {
		t.Error("catalog without models/data accepted")
	}
}

func m(slug string, window int64, hidden bool) Model {
	return Model{Slug: slug, ContextWindow: window, Hidden: hidden}
}

func TestPickLatestPerTier(t *testing.T) {
	sel, err := Pick([]Model{
		m("gpt-6-astra", 1_050_000, false),
		m("gpt-6.1-astra", 1_000_000, false),
		m("gpt-6-sol", 1_050_000, false),
		m("gpt-6.1-sol", 900_000, false),
		m("gpt-6.1-luna", 800_000, false),
		m("gpt-6-luna", 1_050_000, false),
		m("gpt-5.6-luna", 372_000, true), // superseded, hidden in the picker
		m("gpt-5.6-sol", 1_000_000, true),
		m("gpt-5-codex", 272_000, false), // not a tiered model
	})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Fable != "gpt-6.1-astra" || sel.Opus != "gpt-6.1-sol" || sel.Sonnet != "gpt-6.1-luna" {
		t.Errorf("main tiers = %+v", sel)
	}
	// Haiku: luna of the release before the newest luna (6), not 5.6.
	if sel.Haiku != "gpt-6-luna" {
		t.Errorf("Haiku = %q, want gpt-6-luna", sel.Haiku)
	}
	if sel.ContextWindow != 800_000 {
		t.Errorf("ContextWindow = %d, want min(1000000, 900000, 800000)", sel.ContextWindow)
	}
}

func TestPickPreviousLunaMayBeHidden(t *testing.T) {
	sel, err := Pick([]Model{
		m("gpt-6-astra", 1_050_000, false),
		m("gpt-6-sol", 1_050_000, false),
		m("gpt-6-luna", 1_050_000, false),
		m("gpt-5.6-luna", 372_000, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Haiku != "gpt-5.6-luna" || sel.HaikuWindow != 372_000 {
		t.Errorf("Haiku = %q window %d", sel.Haiku, sel.HaikuWindow)
	}
	// Haiku's smaller window does not shrink the main window but is reported.
	if sel.ContextWindow != 1_050_000 {
		t.Errorf("ContextWindow = %d", sel.ContextWindow)
	}
	if len(sel.Notes) != 1 || !strings.Contains(sel.Notes[0], "gpt-5.6-luna") {
		t.Errorf("notes = %v", sel.Notes)
	}
}

func TestPickHiddenNeverNewest(t *testing.T) {
	sel, err := Pick([]Model{
		m("gpt-7-astra", 1, true), // hidden: must not become Fable
		m("gpt-6-astra", 1_050_000, false),
		m("gpt-6-sol", 1_050_000, false),
		m("gpt-6-luna", 1_050_000, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Fable != "gpt-6-astra" {
		t.Errorf("Fable = %q", sel.Fable)
	}
}

func TestPickSingleLunaAndUnknownWindow(t *testing.T) {
	sel, err := Pick([]Model{
		m("gpt-6-astra", 0, false),
		m("gpt-6-sol", 0, false),
		m("gpt-6-luna", 0, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Haiku != "gpt-6-luna" || len(sel.Notes) != 1 {
		t.Errorf("Haiku=%q notes=%v", sel.Haiku, sel.Notes)
	}
	if sel.ContextWindow != DefaultContextWindow {
		t.Errorf("unknown windows -> %d, want %d", sel.ContextWindow, DefaultContextWindow)
	}
}

func TestPickMissingTier(t *testing.T) {
	_, err := Pick([]Model{m("gpt-6-sol", 1, false), m("gpt-6-luna", 1, false)})
	if err == nil || !strings.Contains(err.Error(), "astra") {
		t.Fatalf("err = %v, want missing astra", err)
	}
}

func TestVersionOrder(t *testing.T) {
	a, _ := parseVersion("6.10")
	b, _ := parseVersion("6.9")
	c, _ := parseVersion("6")
	if cmpVersion(a, b) <= 0 || cmpVersion(b, c) <= 0 || cmpVersion(c, c) != 0 {
		t.Errorf("cmp: 6.10 vs 6.9 = %d, 6.9 vs 6 = %d", cmpVersion(a, b), cmpVersion(b, c))
	}
}
