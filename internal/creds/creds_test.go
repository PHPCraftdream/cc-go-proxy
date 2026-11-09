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

func TestTokenIndexRecreateAfterRemoval(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	oldToken, err := s.EnsureProfileToken("work")
	if err != nil {
		t.Fatal(err)
	}
	idx := &TokenIndex{Store: s}
	if _, ok := idx.Lookup(oldToken); !ok {
		t.Fatal("initial token was not indexed")
	}
	if err := os.Remove(s.tokensPath()); err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Lookup(oldToken); ok {
		t.Fatal("token authorized after token file removal")
	}
	newToken, err := s.EnsureProfileToken("work")
	if err != nil {
		t.Fatal(err)
	}
	if newToken == oldToken {
		t.Fatal("recreated token unexpectedly reused old token")
	}
	if p, ok := idx.Lookup(newToken); !ok || p != "work" {
		t.Fatalf("recreated token Lookup = %q, %v", p, ok)
	}
	if _, ok := idx.Lookup(oldToken); ok {
		t.Fatal("old token authorized after file recreation")
	}
}

func TestTokenIndexFailsClosedOnChangedCorruptFile(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tok, err := s.EnsureProfileToken("work")
	if err != nil {
		t.Fatal(err)
	}
	idx := &TokenIndex{Store: s}
	if _, ok := idx.Lookup(tok); !ok {
		t.Fatal("initial token was not indexed")
	}
	if err := os.WriteFile(s.tokensPath(), []byte("tokens: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Lookup(tok); ok {
		t.Fatal("cached token authorized after token file became corrupt")
	}
}

func TestTokenIndexFailsClosedOnStatError(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tok, err := s.EnsureProfileToken("work")
	if err != nil {
		t.Fatal(err)
	}
	idx := &TokenIndex{Store: s}
	if _, ok := idx.Lookup(tok); !ok {
		t.Fatal("initial token was not indexed")
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	idx.Store.Dir = file
	if _, ok := idx.Lookup(tok); ok {
		t.Fatal("cached token authorized after token file stat failed")
	}
}

func TestTokenIndexFailsClosedOnUnreadableFile(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tok, err := s.EnsureProfileToken("work")
	if err != nil {
		t.Fatal(err)
	}
	idx := &TokenIndex{Store: s}
	if _, ok := idx.Lookup(tok); !ok {
		t.Fatal("initial token was not indexed")
	}
	path := s.tokensPath()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Lookup(tok); ok {
		t.Fatal("cached token authorized after token file became unreadable")
	}
}

func TestTokenIndexUnchangedFileDoesNotReread(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tok, err := s.EnsureProfileToken("work")
	if err != nil {
		t.Fatal(err)
	}
	idx := &TokenIndex{Store: s}
	if _, ok := idx.Lookup(tok); !ok {
		t.Fatal("initial token was not indexed")
	}
	path := s.tokensPath()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := "0" + tok[1:]
	if tok[0] == '0' {
		changed = "1" + tok[1:]
	}
	text, err := ktavfile.Encode(tokensFile{Tokens: []profileToken{{Profile: "work", Token: changed}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !os.SameFile(before, after) {
		t.Fatal("could not preserve token file metadata while changing contents")
	}
	for range 3 {
		if p, ok := idx.Lookup(tok); !ok || p != "work" {
			t.Fatalf("cached token Lookup = %q, %v", p, ok)
		}
		if _, ok := idx.Lookup(changed); ok {
			t.Fatal("changed token matched despite unchanged file metadata")
		}
		if _, ok := idx.Lookup("miss"); ok {
			t.Fatal("miss unexpectedly matched")
		}
	}
}

func TestTokenIndexReloadRetriesWhenFileChangesDuringRead(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	intermediate := map[string]string{"middle": "middle-token"}
	latest := map[string]string{"latest": "latest-token"}
	if err := s.writeProfileTokens(map[string]string{"initial": "initial-token"}); err != nil {
		t.Fatal(err)
	}
	reads := 0
	idx := &TokenIndex{
		Store: s,
		readTokens: func() (map[string]string, error) {
			reads++
			m, err := s.ProfileTokens()
			if reads == 2 {
				if writeErr := s.writeProfileTokens(latest); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			return m, err
		},
	}
	if _, ok := idx.Lookup("initial-token"); !ok {
		t.Fatal("initial token was not indexed")
	}
	if err := s.writeProfileTokens(intermediate); err != nil {
		t.Fatal(err)
	}
	if p, ok := idx.Lookup("latest-token"); !ok || p != "latest" {
		t.Fatalf("Lookup(latest token) = %q, %v", p, ok)
	}
	if _, ok := idx.Lookup("middle-token"); ok {
		t.Fatal("stale file contents were associated with newer metadata")
	}
	if reads != 3 {
		t.Fatalf("token file read %d times, want 3 across retry", reads)
	}
}

func TestTokenIndexMigratesLegacyJSON(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	idx := &TokenIndex{Store: s}
	if _, ok := idx.Lookup("legacy-token"); ok {
		t.Fatal("empty store matched")
	}
	legacyPath := filepath.Join(s.Dir, "profile-tokens.json")
	if err := os.WriteFile(legacyPath, []byte(`{"work":"legacy-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, ok := idx.Lookup("legacy-token"); !ok || p != "work" {
		t.Fatalf("Lookup(legacy token) = %q, %v", p, ok)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy JSON file survived migration: %v", err)
	}
}

func TestTokenIndexRejectsDeletedCachedToken(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	tok, err := s.EnsureProfileToken("work")
	if err != nil {
		t.Fatal(err)
	}
	idx := &TokenIndex{Store: s}
	if p, ok := idx.Lookup(tok); !ok || p != "work" {
		t.Fatalf("initial Lookup = %q, %v", p, ok)
	}
	if err := s.DeleteProfileToken("work"); err != nil {
		t.Fatal(err)
	}
	if p, ok := idx.Lookup(tok); ok {
		t.Fatalf("deleted token still authorized as %q", p)
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

func TestHasProviderCreds(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if s.HasProviderCreds("p") {
		t.Fatal("empty store reports a login")
	}
	if err := s.SaveAPIKey("p", "k"); err != nil {
		t.Fatal(err)
	}
	if !s.HasProviderCreds("p") || s.HasProviderCreds("other") {
		t.Fatal("api key not detected, or detected for another provider")
	}
	if err := s.DeleteProviderCreds("p"); err != nil {
		t.Fatal(err)
	}
	// A legacy or unreadable file still counts: logout must be able to remove it.
	if err := os.WriteFile(ktavfile.LegacyPath(s.OAuthPath("p")), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !s.HasProviderCreds("p") {
		t.Fatal("legacy file not detected")
	}
}
