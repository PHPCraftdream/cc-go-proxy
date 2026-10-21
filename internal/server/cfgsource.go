package server

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

// cfgSource reloads config.ktav when it changes, so profiles and mappings
// edited while the server runs apply without restart. Listen address changes
// still need a restart.
type cfgSource struct {
	mu  sync.Mutex
	cfg *config.Config
	mod time.Time
}

func newCfgSource(cfg *config.Config) *cfgSource {
	s := &cfgSource{cfg: cfg}
	if st, err := os.Stat(cfg.ConfigFile); err == nil {
		s.mod = st.ModTime()
	}
	return s
}

func (s *cfgSource) get() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.IsEnvMode() {
		return s.cfg
	}
	st, err := os.Stat(s.cfg.ConfigFile)
	if err != nil || st.ModTime().Equal(s.mod) {
		return s.cfg
	}
	next, err := config.Load()
	if err != nil {
		fmt.Printf("[WARN] config reload failed, keeping previous: %v\n", err)
		s.mod = st.ModTime() // don't retry until the file changes again
		return s.cfg
	}
	next.Debug, next.SimpleLog = s.cfg.Debug, s.cfg.SimpleLog
	next.Host, next.Port = s.cfg.Host, s.cfg.Port
	s.cfg, s.mod = next, st.ModTime()
	return s.cfg
}
