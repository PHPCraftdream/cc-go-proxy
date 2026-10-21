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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/PHPCraftdream/cc-go-proxy/internal/securefile"
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

func (s Store) tokensPath() string { return filepath.Join(s.Dir, "profile-tokens.json") }

// OAuthPath is the OAuth token file for a provider.
func (s Store) OAuthPath(provider string) string {
	return filepath.Join(s.Dir, "oauth-"+provider+".json")
}

func (s Store) apiKeyPath(provider string) string {
	return filepath.Join(s.Dir, "apikey-"+provider+".json")
}

// ProfileTokens returns profile name -> URL token.
func (s Store) ProfileTokens() (map[string]string, error) {
	m := map[string]string{}
	if err := readJSON(s.tokensPath(), &m); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return m, nil
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
	if err := writeJSON(s.tokensPath(), m); err != nil {
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
	return writeJSON(s.tokensPath(), m)
}

// APIKey returns the stored key for a provider; "" if absent.
func (s Store) APIKey(provider string) (string, error) {
	var v struct {
		APIKey string `json:"api_key"`
	}
	err := readJSON(s.apiKeyPath(provider), &v)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return v.APIKey, err
}

// DeleteProviderCreds removes a provider's OAuth and API-key files. Missing
// files are fine: removal is idempotent.
func (s Store) DeleteProviderCreds(provider string) error {
	for _, p := range []string{s.OAuthPath(provider), s.apiKeyPath(provider)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// SaveAPIKey stores a provider key owner-only.
func (s Store) SaveAPIKey(provider, key string) error {
	return writeJSON(s.apiKeyPath(provider), map[string]string{"api_key": key})
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("corrupt %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return securefile.WriteFile(path, b)
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
