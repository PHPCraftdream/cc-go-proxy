package catalog

import (
	"strings"
	"testing"
)

func TestFastServiceTierCatalogShapes(t *testing.T) {
	models, err := ParseCatalog([]byte(`{"models":[
		{"slug":"spaced","service_tiers":[{"id":"p1","name":"  fast "}]},
		{"slug":"blank-id-first","service_tiers":[{"id":"","name":"Fast"},{"id":"p2","name":"Fast"}]},
		{"slug":"null-tiers","service_tiers":null},
		{"slug":"no-field"},
		{"slug":"broken","service_tiers":"oops"},
		{"slug":"standard-only","service_tiers":[{"id":"s","name":"Standard"}]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	for slug, want := range map[string]string{"spaced": "p1", "blank-id-first": "p2"} {
		if got, err := FastServiceTier(models, slug); err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", slug, got, err, want)
		}
	}
	for slug, msg := range map[string]string{
		"null-tiers":    "does not advertise",
		"no-field":      "does not advertise",
		"standard-only": "does not advertise",
		"broken":        "invalid service tiers",
		"absent":        "absent from the account catalog",
	} {
		if got, err := FastServiceTier(models, slug); err == nil || !strings.Contains(err.Error(), msg) || got != "" {
			t.Errorf("%s: %q, %v; want an error with %q", slug, got, err, msg)
		}
	}
}
