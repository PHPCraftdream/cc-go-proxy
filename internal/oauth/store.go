package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/ktavfile"
)

// ErrNotLoggedIn: no stored tokens for this provider.
var ErrNotLoggedIn = errors.New("oauth: not logged in, run `login` first")

// Store persists Tokens as Ktav readable only by the current user.
type Store struct {
	Path string
}

func (s Store) Load() (*Tokens, error) {
	unlock, err := creds.LockFile(context.Background(), s.Path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.load()
}

func (s Store) load() (*Tokens, error) {
	var t Tokens
	err := ktavfile.Read(s.Path, &t, json.Unmarshal) // also picks up the oauth-*.json of earlier versions
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotLoggedIn
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: %w", err)
	}
	if t.RefreshToken == "" {
		return nil, ErrNotLoggedIn
	}
	return &t, nil
}

// Save writes atomically, owner-only.
func (s Store) Save(t *Tokens) error {
	unlock, err := creds.LockFile(context.Background(), s.Path)
	if err != nil {
		return err
	}
	defer unlock()
	return s.save(t)
}

func (s Store) save(t *Tokens) error {
	return ktavfile.Write(s.Path, t)
}

// Session serves fresh access tokens, refreshing and persisting rotation.
// Safe for concurrent use; refreshes are serialized.
type Session struct {
	Store  Store
	Client *Client

	mu     sync.Mutex
	tokens *Tokens
}

// Token returns valid tokens, refreshing when close to expiry.
func (s *Session) Token(ctx context.Context) (*Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := creds.LockFile(ctx, s.Store.Path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	if !NeedsRefresh(s.tokens, time.Now()) {
		return s.tokens, nil
	}
	return s.refreshLocked(ctx)
}

// ForceRefresh refreshes after an upstream 401. Skipped if the access token
// already changed since staleAccessToken was handed out.
func (s *Session) ForceRefresh(ctx context.Context, staleAccessToken string) (*Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := creds.LockFile(ctx, s.Store.Path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	if s.tokens.AccessToken != staleAccessToken {
		return s.tokens, nil
	}
	return s.refreshLocked(ctx)
}

// reloadLocked unconditionally reloads the file; the caller holds the file lock.
func (s *Session) reloadLocked() error {
	t, err := s.Store.load()
	if err != nil {
		return err
	}
	s.tokens = t
	return nil
}

func (s *Session) refreshLocked(ctx context.Context) (*Tokens, error) {
	next, err := s.Client.Refresh(ctx, s.tokens)
	if err != nil {
		return nil, err
	}
	// Persist before use: the old refresh token is now invalid.
	if err := s.Store.save(next); err != nil {
		return nil, fmt.Errorf("oauth: save rotated tokens: %w", err)
	}
	s.tokens = next
	return next, nil
}
