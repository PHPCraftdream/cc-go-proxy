package creds

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/ktavfile"
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

func TestDeleteProfileToken(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tok, err := s.EnsureProfileToken("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfileToken("other"); err != nil {
		t.Errorf("missing entry: %v", err)
	}
	if err := s.DeleteProfileToken("work"); err != nil {
		t.Fatal(err)
	}
	m, err := s.ProfileTokens()
	if err != nil || m["work"] != "" {
		t.Fatalf("tokens after delete = %v, %v", m, err)
	}
	if err := s.DeleteProfileToken("work"); err != nil {
		t.Errorf("second delete is not idempotent: %v", err)
	}
	if tok == "" {
		t.Error("sanity")
	}
}

func TestDeleteProviderCreds(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.DeleteProviderCreds("gone"); err != nil {
		t.Fatalf("missing files: %v", err)
	}
	if err := s.SaveAPIKey("oai", "sk-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.OAuthPath("oai"), []byte("k: v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := ktavfile.LegacyPath(s.OAuthPath("oai"))
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProviderCreds("oai"); err != nil {
		t.Fatal(err)
	}
	if k, err := s.APIKey("oai"); err != nil || k != "" {
		t.Fatalf("key after delete = %q, %v", k, err)
	}
	if _, err := os.Stat(s.OAuthPath("oai")); !os.IsNotExist(err) {
		t.Errorf("oauth file survived: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy json survived: %v", err)
	}
	if err := s.DeleteProviderCreds("oai"); err != nil {
		t.Errorf("second delete is not idempotent: %v", err)
	}
}

// The files of earlier versions were JSON; they are converted on first read and removed.
func TestLegacyJSONIsMigrated(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	oldTokens := filepath.Join(s.Dir, "profile-tokens.json")
	if err := os.WriteFile(oldTokens, []byte(`{"codex": "tok-1", "my.profile": "1234", "zai": "tok-3"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldKey := filepath.Join(s.Dir, "apikey-zai.json")
	if err := os.WriteFile(oldKey, []byte(`{"api_key": "sk-old"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := s.ProfileTokens()
	if err != nil || len(m) != 3 || m["codex"] != "tok-1" || m["my.profile"] != "1234" || m["zai"] != "tok-3" {
		t.Fatalf("tokens = %v, %v", m, err)
	}
	if k, err := s.APIKey("zai"); err != nil || k != "sk-old" {
		t.Fatalf("key = %q, %v", k, err)
	}
	for _, old := range []string{oldTokens, oldKey} {
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Errorf("%s was not removed: %v", filepath.Base(old), err)
		}
	}
	// Now the Ktav files are the only source.
	m2, err := s.ProfileTokens()
	if err != nil || len(m2) != 3 || m2["my.profile"] != "1234" {
		t.Errorf("tokens after migration = %v, %v", m2, err)
	}
}

func TestTokensSurviveOddProfileNames(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	names := []string{"codex", "my.profile", "123", "a b", "ключ", "x: y", "#hash"}
	want := map[string]string{}
	for _, n := range names {
		tok, err := s.EnsureProfileToken(n)
		if err != nil {
			t.Fatalf("%q: %v", n, err)
		}
		want[n] = tok
	}
	got, err := s.ProfileTokens()
	if err != nil || len(got) != len(names) {
		t.Fatalf("tokens = %v, %v", got, err)
	}
	for n, tok := range want {
		if got[n] != tok {
			t.Errorf("%q: got %q, want %q", n, got[n], tok)
		}
	}
	// Deleting the last ones leaves a readable, empty store.
	for _, n := range names {
		if err := s.DeleteProfileToken(n); err != nil {
			t.Fatalf("delete %q: %v", n, err)
		}
	}
	if got, err := s.ProfileTokens(); err != nil || len(got) != 0 {
		t.Errorf("tokens after deleting all = %v, %v", got, err)
	}
}

func TestAPIKeyRoundTripsNastyValues(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	for _, key := range []string{"sk-proj-abc_DEF-123", "123456", "1e5", "true", "null", "  padded  ", "a: b", "# not a comment", "line1\nline2", "ключ"} {
		if err := s.SaveAPIKey("p", key); err != nil {
			t.Fatalf("save %q: %v", key, err)
		}
		if got, err := s.APIKey("p"); err != nil || got != key {
			t.Errorf("key %q read back as %q, %v", key, got, err)
		}
	}
}

func TestCorruptCredsFileIsAnError(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(s.Dir, "apikey-p.ktav"), []byte("api_key: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.APIKey("p"); err == nil {
		t.Error("corrupt key file read as empty")
	}
}
