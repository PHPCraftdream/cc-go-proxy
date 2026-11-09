// Package creds stores machine-generated secrets: per-profile URL tokens,
// provider API keys and OAuth sessions. Never edited by hand.
package creds

import (
	"context"
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
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
	"github.com/PHPCraftdream/cc-go-proxy/internal/ktavfile"
)

// Store is rooted at a creds directory.
type Store struct {
	Dir string
}

// DefaultDir returns <UserConfigDir>/cc-go-proxy/creds, or creds in the home directory.
func DefaultDir() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "creds"), nil
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
	path := s.tokensPath()
	if _, err := os.Stat(path); err == nil {
		return s.profileTokensUnlocked()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	legacyPath := ktavfile.LegacyPath(path)
	if _, err := os.Stat(legacyPath); errors.Is(err, fs.ErrNotExist) {
		return s.profileTokensUnlocked()
	} else if err != nil {
		return nil, err
	}

	// Reading a legacy JSON file migrates it, so serialize that slow-path write.
	unlock, err := LockFile(context.Background(), path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.profileTokensUnlocked()
}

// profileTokensUnlocked is only used while the current token file is known to
// exist or while holding its sidecar lock (legacy migration may write).
func (s Store) profileTokensUnlocked() (map[string]string, error) {
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
	unlock, err := LockFile(context.Background(), s.tokensPath())
	if err != nil {
		return "", err
	}
	defer unlock()

	m, err := s.profileTokensUnlocked()
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
	unlock, err := LockFile(context.Background(), s.tokensPath())
	if err != nil {
		return err
	}
	defer unlock()

	m, err := s.profileTokensUnlocked()
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

// HasProviderCreds reports whether any stored login file of the provider
// (OAuth tokens or API key, current or legacy format) exists, valid or not.
func (s Store) HasProviderCreds(provider string) bool {
	for _, p := range []string{s.OAuthPath(provider), s.apiKeyPath(provider)} {
		for _, f := range []string{p, ktavfile.LegacyPath(p)} {
			if _, err := os.Stat(f); err == nil {
				return true
			}
		}
	}
	return false
}

// SaveAPIKey stores a provider key owner-only.
func (s Store) SaveAPIKey(provider, key string) error {
	return ktavfile.Write(s.apiKeyPath(provider), apiKeyFile{APIKey: key})
}

// TokenIndex resolves URL tokens to profile names, refreshing its cached hashes when token files change.
type TokenIndex struct {
	Store Store

	mu         sync.Mutex
	hashes     map[string][32]byte // profile -> sha256(token)
	stamp      tokenFilesStamp
	loaded     bool
	readTokens func() (map[string]string, error) // nil uses Store.ProfileTokens
	static     bool                              // fixed token, never reload
}

// Lookup returns the profile whose token equals tok. It checks the token file's
// metadata before using cached hashes, and reloads only when the file changes.
func (x *TokenIndex) Lookup(tok string) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	h := sha256.Sum256([]byte(tok))
	if x.static {
		return x.match(h)
	}
	stamp, err := x.tokenFilesStamp()
	if err != nil {
		x.clear()
		return "", false
	}
	if x.loaded && x.stamp.equal(stamp) {
		return x.match(h)
	}
	if x.reload(stamp) {
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

// reload reads a stable file snapshot. If the file changes while it is being
// read, retry from the new metadata; never associate stale contents with a
// newer stamp. A stable read error is cached as an empty index until metadata
// changes, while a stat error always clears the index and fails closed.
func (x *TokenIndex) reload(before tokenFilesStamp) bool {
	for attempt := 0; attempt < 3; attempt++ {
		m, readErr := x.loadProfileTokens()
		after, statErr := x.tokenFilesStamp()
		if statErr != nil {
			x.clear()
			return false
		}
		if !before.equal(after) {
			before = after
			continue
		}
		if readErr != nil {
			x.hashes = make(map[string][32]byte)
			x.stamp = after
			x.loaded = true
			return false
		}
		hashes := make(map[string][32]byte, len(m))
		for p, t := range m {
			hashes[p] = sha256.Sum256([]byte(t))
		}
		x.hashes = hashes
		x.stamp = after
		x.loaded = true
		return true
	}
	x.clear()
	return false
}

func (x *TokenIndex) loadProfileTokens() (map[string]string, error) {
	if x.readTokens != nil {
		return x.readTokens()
	}
	return x.Store.ProfileTokens()
}

func (x *TokenIndex) clear() {
	x.hashes = nil
	x.stamp = tokenFilesStamp{}
	x.loaded = false
}

type tokenFilesStamp struct {
	current tokenFileStamp
	legacy  tokenFileStamp
}

func (s tokenFilesStamp) equal(other tokenFilesStamp) bool {
	return s.current.equal(other.current) && s.legacy.equal(other.legacy)
}

type tokenFileStamp struct {
	exists  bool
	size    int64
	modTime time.Time
	info    os.FileInfo
}

func (s tokenFileStamp) equal(other tokenFileStamp) bool {
	if s.exists != other.exists {
		return false
	}
	if !s.exists {
		return true
	}
	return s.size == other.size && s.modTime.Equal(other.modTime) && os.SameFile(s.info, other.info)
}

func (x *TokenIndex) tokenFilesStamp() (tokenFilesStamp, error) {
	current, err := statTokenFile(x.Store.tokensPath())
	if err != nil {
		return tokenFilesStamp{}, err
	}
	legacy, err := statTokenFile(ktavfile.LegacyPath(x.Store.tokensPath()))
	if err != nil {
		return tokenFilesStamp{}, err
	}
	return tokenFilesStamp{current: current, legacy: legacy}, nil
}

func statTokenFile(path string) (tokenFileStamp, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return tokenFileStamp{}, nil
	}
	if err != nil {
		return tokenFileStamp{}, err
	}
	return tokenFileStamp{exists: true, size: info.Size(), modTime: info.ModTime(), info: info}, nil
}

// StaticIndex maps a single fixed token to a profile (env-only mode).
func StaticIndex(profile, token string) *TokenIndex {
	return &TokenIndex{
		hashes: map[string][32]byte{profile: sha256.Sum256([]byte(token))},
		static: true,
	}
}
