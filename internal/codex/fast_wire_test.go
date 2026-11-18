package codex

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFastWireCatalogAndSelection(t *testing.T) {
	models, err := ParseCatalog([]byte(`{"models":[{"slug":"example","service_tiers":[{"id":"custom-priority","name":"Fast"}],"additional_speed_tiers":["fast"]},{"slug":"no-tier","additional_speed_tiers":["fast"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := FastServiceTier(models, "example")
	if err != nil || id != "custom-priority" {
		t.Fatalf("tier = %q, %v", id, err)
	}
	for _, slug := range []string{"missing", "no-tier"} {
		if _, err := FastServiceTier(models, slug); err == nil {
			t.Fatalf("%s accepted", slug)
		}
	}
	for _, tc := range []struct {
		speed string
		fast  bool
	}{{`"fast"`, true}, {`"standard"`, false}, {`null`, false}, {`"other"`, false}} {
		p, err := ParseRequest([]byte(`{"model":"x","speed":` + tc.speed + `}`))
		if err != nil || p.ClientFast() != tc.fast {
			t.Fatalf("speed %s: %v", tc.speed, err)
		}
	}
	p, err := ParseRequest([]byte(`{"model":"x"}`))
	if err != nil || p.ClientFast() {
		t.Fatal("absent speed selected fast")
	}
	req := &Request{Model: "example", ServiceTier: id}
	for _, ws := range []bool{false, true} {
		var body []byte
		if ws {
			body, err = WSCreatePayload(req, "previous", nil)
		} else {
			body, err = json.Marshal(req)
		}
		if err != nil || !strings.Contains(string(body), `"service_tier":"custom-priority"`) {
			t.Fatalf("wire %s: %v", body, err)
		}
	}
	req.ServiceTier = ""
	body, err := json.Marshal(req)
	if err != nil || strings.Contains(string(body), "service_tier") {
		t.Fatalf("standard wire %s: %v", body, err)
	}
}

func TestFastTierChangeForcesFullWebsocket(t *testing.T) {
	chain := &WSChain{responseID: "id", serviceTier: "priority"}
	_, ok, reason := chain.Delta(&Request{PromptCacheKey: "key"})
	if ok || reason != "service tier changed" {
		t.Fatalf("delta %v: %s", ok, reason)
	}
}
