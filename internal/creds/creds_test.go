package creds

import (
	"testing"
)

func TestEnsureProfileTokenStable(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	a, err := s.EnsureProfileToken("work")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 {
		t.Errorf("token length = %d, want 64", len(a))
	}
	b, err := s.EnsureProfileToken("work")
	if err != nil || a != b {
		t.Fatalf("second call = %q, %v; want %q", b, err, a)
	}
	c, _ := s.EnsureProfileToken("other")
	if c == a {
		t.Error("profiles share a token")
	}
}

func TestAPIKeyRoundTrip(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if k, err := s.APIKey("oai"); k != "" || err != nil {
		t.Fatalf("missing key = %q, %v", k, err)
	}
	if err := s.SaveAPIKey("oai", "sk-1"); err != nil {
		t.Fatal(err)
	}
	if k, err := s.APIKey("oai"); k != "sk-1" || err != nil {
		t.Fatalf("key = %q, %v", k, err)
	}
}

func TestTokenIndexReloadsOnMiss(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	idx := &TokenIndex{Store: s}
	if _, ok := idx.Lookup("anything"); ok {
		t.Fatal("empty store matched")
	}
	tok, err := s.EnsureProfileToken("work")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := idx.Lookup(tok); !ok || p != "work" {
		t.Fatalf("Lookup(new token) = %q, %v", p, ok)
	}

	// Added while running: picked up on miss.
	tok2, _ := s.EnsureProfileToken("agents")
	if p, ok := idx.Lookup(tok2); !ok || p != "agents" {
		t.Fatalf("Lookup(second token) = %q, %v", p, ok)
	}
	if _, ok := idx.Lookup(tok + "x"); ok {
		t.Error("near-miss token matched")
	}
}

func TestStaticIndex(t *testing.T) {
	idx := StaticIndex("default", "secret-token")
	if p, ok := idx.Lookup("secret-token"); !ok || p != "default" {
		t.Fatalf("Lookup = %q, %v", p, ok)
	}
	if _, ok := idx.Lookup("other"); ok {
		t.Error("static index matched wrong token")
	}
}
