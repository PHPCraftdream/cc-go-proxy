package shared

import (
	"sync"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

// sessions caches one OAuth session per codex provider.
type Sessions struct {
	store creds.Store
	mu    sync.Mutex
	m     map[string]*oauth.Session
	peak  *peak.Pauses
}

// pauses are the pauses of peak hours (`ccgp --peak <provider> pause`), kept in
// the creds directory.
func (s *Sessions) Pauses() *peak.Pauses {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peak == nil {
		s.peak = peak.NewPauses(s.store.Dir)
	}
	return s.peak
}

func (s *Sessions) Get(provider string) *oauth.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*oauth.Session{}
	}
	if ss, ok := s.m[provider]; ok {
		return ss
	}
	ss := &oauth.Session{Store: oauth.Store{Path: s.store.OAuthPath(provider)}, Client: &oauth.Client{}}
	s.m[provider] = ss
	return ss
}

// NewSessions returns the Codex sessions of the logins kept in store.
func NewSessions(store creds.Store) *Sessions { return &Sessions{store: store} }
