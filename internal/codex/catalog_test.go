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
		m("gpt-6.1-terra", 700_000, false),
		m("gpt-5.6-terra", 272_000, false),
		m("gpt-6.1-luna", 800_000, false),
		m("gpt-6-luna", 1_050_000, false),
		m("gpt-5.6-luna", 372_000, true), // superseded, hidden in the picker
		m("gpt-5.6-sol", 1_000_000, true),
		m("gpt-5-codex", 272_000, false), // not a tiered model
	})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Fable != "gpt-6.1-astra" || sel.Opus != "gpt-6.1-sol" || sel.Sonnet != "gpt-6.1-terra" || sel.Haiku != "gpt-6.1-luna" {
		t.Errorf("tiers = %+v", sel)
	}
	if sel.FableWindow != 1_000_000 || sel.OpusWindow != 900_000 || sel.SonnetWindow != 700_000 || sel.HaikuWindow != 800_000 {
		t.Errorf("windows = %d %d %d %d", sel.FableWindow, sel.OpusWindow, sel.SonnetWindow, sel.HaikuWindow)
	}
	if len(sel.Notes) != 0 {
		t.Errorf("unexpected notes = %v", sel.Notes)
	}
}

// The catalog of 2026-10-26: gpt-6.1-terra is new, luna is still at 6.
func TestPickCatalogWithTerra(t *testing.T) {
	sel, err := Pick([]Model{
		m("gpt-6.1-sol", 500_000, false),
		m("gpt-6-astra", 272_000, false),
		m("gpt-6.1-terra", 500_000, false),
		m("gpt-6-sol", 272_000, false),
		m("gpt-6-luna", 272_000, false),
		m("gpt-5.6-sol", 272_000, false),
		m("gpt-5.6-terra", 272_000, false),
		m("gpt-5.6-luna", 272_000, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Fable != "gpt-6-astra" || sel.Opus != "gpt-6.1-sol" || sel.Sonnet != "gpt-6.1-terra" || sel.Haiku != "gpt-6-luna" {
		t.Errorf("tiers = %+v", sel)
	}
}

func TestPickSonnetFallsBackToLuna(t *testing.T) {
	sel, err := Pick([]Model{
		m("gpt-6-astra", 1_050_000, false),
		m("gpt-6-sol", 1_050_000, false),
		m("gpt-6-luna", 1_050_000, false),
		m("gpt-5.6-terra", 272_000, true), // hidden: not a candidate
	})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Sonnet != "gpt-6-luna" || sel.Haiku != "gpt-6-luna" || sel.SonnetWindow != 1_050_000 {
		t.Errorf("Sonnet %q (%d), Haiku %q", sel.Sonnet, sel.SonnetWindow, sel.Haiku)
	}
	if !strings.Contains(strings.Join(sel.Notes, "|"), "no terra release") {
		t.Errorf("notes = %v", sel.Notes)
	}
}

func TestPickHiddenNeverNewest(t *testing.T) {
	sel, err := Pick([]Model{
		m("gpt-7-astra", 1, true), // hidden: must not become Fable
		m("gpt-6-astra", 1_050_000, false),
		m("gpt-6-sol", 1_050_000, false),
		m("gpt-7-terra", 1, true),
		m("gpt-6-terra", 1_050_000, false),
		m("gpt-6-luna", 1_050_000, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Fable != "gpt-6-astra" || sel.Sonnet != "gpt-6-terra" {
		t.Errorf("Fable = %q, Sonnet = %q", sel.Fable, sel.Sonnet)
	}
}

func TestPickUnknownWindow(t *testing.T) {
	sel, err := Pick([]Model{
		m("gpt-6-astra", 0, false),
		m("gpt-6-sol", 0, false),
		m("gpt-6-terra", 0, false),
		m("gpt-6-luna", 0, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, w := range map[string]int64{"fable": sel.FableWindow, "opus": sel.OpusWindow, "sonnet": sel.SonnetWindow, "haiku": sel.HaikuWindow} {
		if w != DefaultContextWindow {
			t.Errorf("%s window = %d, want the default %d", name, w, DefaultContextWindow)
		}
	}
	if !strings.Contains(strings.Join(sel.Notes, "|"), "gives no context window for gpt-6-sol") {
		t.Errorf("notes = %v", sel.Notes)
	}
}

func TestPickMissingTier(t *testing.T) {
	_, err := Pick([]Model{m("gpt-6-sol", 1, false), m("gpt-6-luna", 1, false)})
	if err == nil || !strings.Contains(err.Error(), "astra") {
		t.Fatalf("err = %v, want missing astra", err)
	}
	_, err = Pick([]Model{m("gpt-6-astra", 1, false), m("gpt-6-sol", 1, false), m("gpt-6-terra", 1, false)})
	if err == nil || !strings.Contains(err.Error(), "luna (Haiku)") {
		t.Fatalf("err = %v, want missing luna", err)
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

func mm(slug string, window, max int64, hidden bool) Model {
	return Model{Slug: slug, ContextWindow: window, MaxContextWindow: max, Hidden: hidden}
}

// The catalog's context_window is only the default of Codex clients; the GPT-6
// generation really takes up to its documented input limit (922000, above the
// catalog's 872000), earlier releases stay at the default (same max_context_window
// in the catalog, but not usable).
func TestPickWindowsFollowGenerations(t *testing.T) {
	sel, err := Pick([]Model{
		mm("gpt-6-astra", 272_000, 872_000, false),
		mm("gpt-6.1-sol", 500_000, 872_000, false),
		mm("gpt-6-sol", 272_000, 872_000, false),
		mm("gpt-6-terra", 272_000, 872_000, false),
		mm("gpt-5.6-terra", 272_000, 872_000, false),
		mm("gpt-5.6-luna", 272_000, 872_000, false), // the only luna: Haiku is an earlier generation
	})
	if err != nil {
		t.Fatal(err)
	}
	if sel.Fable != "gpt-6-astra" || sel.Opus != "gpt-6.1-sol" || sel.Sonnet != "gpt-6-terra" || sel.Haiku != "gpt-5.6-luna" {
		t.Fatalf("selection = %+v", sel)
	}
	if sel.FableWindow != Gpt6InputLimit || sel.OpusWindow != Gpt6InputLimit || sel.SonnetWindow != Gpt6InputLimit || Gpt6InputLimit != 922_000 {
		t.Errorf("GPT-6 windows = %d %d %d, want %d", sel.FableWindow, sel.OpusWindow, sel.SonnetWindow, Gpt6InputLimit)
	}
	if sel.HaikuWindow != 272_000 {
		t.Errorf("previous-generation window = %d, want the 272000 default", sel.HaikuWindow)
	}
}

// A catalog that later advertises more than the documented limit is trusted.
func TestPickKeepsALargerCatalogMax(t *testing.T) {
	sel, err := Pick([]Model{
		mm("gpt-6.1-sol", 500_000, 1_050_000, false),
		mm("gpt-6-astra", 272_000, 1_050_000, false),
		mm("gpt-6-terra", 272_000, 700_000, false),
		mm("gpt-5.6-luna", 272_000, 1_050_000, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sel.OpusWindow != 1_050_000 || sel.FableWindow != 1_050_000 {
		t.Errorf("windows = %d %d, want the catalog's 1050000", sel.OpusWindow, sel.FableWindow)
	}
	// A catalog max below the documented input limit is raised to it.
	if sel.SonnetWindow != Gpt6InputLimit {
		t.Errorf("sonnet window = %d, want %d", sel.SonnetWindow, Gpt6InputLimit)
	}
	if sel.HaikuWindow != 272_000 {
		t.Errorf("previous-generation window = %d, want the 272000 default", sel.HaikuWindow)
	}
}

func TestParseCatalogReadsMaxContextWindow(t *testing.T) {
	ms, err := ParseCatalog([]byte(`{"models":[{"slug":"gpt-6-luna","context_window":272000,"max_context_window":872000}]}`))
	if err != nil || len(ms) != 1 {
		t.Fatalf("ParseCatalog = %v, %v", ms, err)
	}
	if ms[0].ContextWindow != 272000 || ms[0].MaxContextWindow != 872000 || ms[0].Raw["max_context_window"] == nil {
		t.Errorf("model = %+v", ms[0])
	}
}
