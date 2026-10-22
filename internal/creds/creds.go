// Package creds stores machine-generated secrets: per-profile URL tokens,
// provider API keys and OAuth sessions. Never edited by hand.
package creds

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/PHPCraftdream/cc-go-proxy/internal/ktavfile"
)

// Store is rooted at a creds directory.
type Store struct {
	Dir string
}

// DefaultDir returns <UserConfigDir>/cc-go-proxy/creds.
func DefaultDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cc-go-proxy", "creds"), nil
}

func (s Store) tokensPath() string { return filepath.Join(s.Dir, "profile-tokens"+ktavfile.Ext) }

// OAuthPath is the OAuth token file for a provider.
func (s Store) OAuthPath(provider string) string {
	return filepath.Join(s.Dir, "oauth-"+provider+ktavfile.Ext)
}

func (s Store) apiKeyPath(provider string) string {
	return filepath.Join(s.Dir, "apikey-"+provider+ktavfile.Ext)
}

// ProfileTokens returns profile name -> URL token.
func (s Store) ProfileTokens() (map[string]string, error) {
	var f tokensFile
	if err := ktavfile.Read(s.tokensPath(), &f, legacyTokens); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	m := make(map[string]string, len(f.Tokens))
	for _, e := range f.Tokens {
		m[e.Profile] = e.Token
	}
	return m, nil
}

// tokensFile is profile-tokens.ktav: a list, since profile names are free text
// and a Ktav key with a dot would read back as nesting.
type tokensFile struct {
	Tokens []profileToken `json:"tokens"`
}

type profileToken struct {
	Profile string `json:"profile"`
	Token   string `json:"token"`
}

// legacyTokens reads the profile-tokens.json of earlier versions (name -> token).
func legacyTokens(data []byte, v any) error {
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	f := v.(*tokensFile)
	f.Tokens = nil
	for p, t := range m {
		f.Tokens = append(f.Tokens, profileToken{Profile: p, Token: t})
	}
	sort.Slice(f.Tokens, func(i, j int) bool { return f.Tokens[i].Profile < f.Tokens[j].Profile })
	return nil
}

func (s Store) writeProfileTokens(m map[string]string) error {
	f := tokensFile{Tokens: make([]profileToken, 0, len(m))}
	for p, t := range m {
		f.Tokens = append(f.Tokens, profileToken{Profile: p, Token: t})
	}
	sort.Slice(f.Tokens, func(i, j int) bool { return f.Tokens[i].Profile < f.Tokens[j].Profile })
	return ktavfile.Write(s.tokensPath(), f)
}

// EnsureProfileToken returns the profile's token, generating it on first use.
func (s Store) EnsureProfileToken(profile string) (string, error) {
	m, err := s.ProfileTokens()
	if err != nil {
		return "", err
	}
	if t := m[profile]; t != "" {
		return t, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	t := hex.EncodeToString(b)
	m[profile] = t
	if err := s.writeProfileTokens(m); err != nil {
		return "", err
	}
	return t, nil
}

// DeleteProfileToken drops a profile's URL token. A missing entry or file is
// not an error: removal is idempotent.
func (s Store) DeleteProfileToken(profile string) error {
	m, err := s.ProfileTokens()
	if err != nil {
		return err
	}
	if _, ok := m[profile]; !ok {
		return nil
	}
	delete(m, profile)
	return s.writeProfileTokens(m)
}

// APIKey returns the stored key for a provider; "" if absent.
func (s Store) APIKey(provider string) (string, error) {
	var v apiKeyFile
	err := ktavfile.Read(s.apiKeyPath(provider), &v, json.Unmarshal)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return v.APIKey, err
}

type apiKeyFile struct {
	APIKey string `json:"api_key"`
}

// DeleteProviderCreds removes a provider's OAuth and API-key files, and the
// JSON ones of earlier versions that were never read again. Missing files are
// fine: removal is idempotent.
func (s Store) DeleteProviderCreds(provider string) error {
	for _, p := range []string{s.OAuthPath(provider), s.apiKeyPath(provider)} {
		for _, f := range []string{p, ktavfile.LegacyPath(p)} {
			if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// SaveAPIKey stores a provider key owner-only.
func (s Store) SaveAPIKey(provider, key string) error {
	return ktavfile.Write(s.apiKeyPath(provider), apiKeyFile{APIKey: key})
}

// TokenIndex resolves URL tokens to profile names; reloads on miss.
type TokenIndex struct {
	Store Store

	mu     sync.Mutex
	hashes map[string][32]byte // profile -> sha256(token)
	static bool                // fixed token, never reload
}

// Lookup returns the profile whose token equals tok. Compares all entries in
// constant time per entry; reloads the store once on miss.
func (x *TokenIndex) Lookup(tok string) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	h := sha256.Sum256([]byte(tok))
	if p, ok := x.match(h); ok {
		return p, true
	}
	if x.reload() {
		return x.match(h)
	}
	return "", false
}

func (x *TokenIndex) match(h [32]byte) (string, bool) {
	found := ""
	for p, want := range x.hashes {
		if subtle.ConstantTimeCompare(h[:], want[:]) == 1 {
			found = p
		}
	}
	return found, found != ""
}

// reload re-reads the token file. Misses are rare (new profile or bad
// token on loopback), so no change detection.
func (x *TokenIndex) reload() bool {
	if x.static {
		return false
	}
	m, err := x.Store.ProfileTokens()
	if err != nil {
		return false
	}
	x.hashes = make(map[string][32]byte, len(m))
	for p, t := range m {
		x.hashes[p] = sha256.Sum256([]byte(t))
	}
	return true
}

// StaticIndex maps a single fixed token to a profile (env-only mode).
func StaticIndex(profile, token string) *TokenIndex {
	return &TokenIndex{
		hashes: map[string][32]byte{profile: sha256.Sum256([]byte(token))},
		static: true,
	}
}
