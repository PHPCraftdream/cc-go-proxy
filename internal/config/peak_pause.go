package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/ktavfile"
)

// pauseFile holds, per provider, the moment until which its peak hours let
// requests through (`ccgp --peak <provider> pause`). It is state of the
// running setup, not configuration, so it lives in the creds directory next to
// the other machine-managed files and config.ktav stays as written.
const pauseFile = "peak-pause" + ktavfile.Ext

// PeakPauses is that file. The CLI writes it; the server reads it when a
// request falls in peak hours (a small file, read afresh, so a change counts at
// once).
type PeakPauses struct {
	path string
	mu   sync.Mutex
}

// NewPeakPauses opens the pauses kept in dir. An empty dir keeps none.
func NewPeakPauses(dir string) *PeakPauses {
	if dir == "" {
		return &PeakPauses{}
	}
	return &PeakPauses{path: filepath.Join(dir, pauseFile)}
}

// read returns the pauses on disk; the caller holds p.mu.
func (p *PeakPauses) read() map[string]time.Time {
	m := map[string]time.Time{}
	if p.path == "" {
		return m
	}
	var raw map[string]string
	if ktavfile.Read(p.path, &raw, nil) != nil {
		return m
	}
	for provider, s := range raw {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			m[provider] = t
		}
	}
	return m
}

// Until is the end of the provider's pause when one is running at now.
func (p *PeakPauses) Until(provider string, now time.Time) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.read()[provider]; ok && t.After(now) {
		return t, true
	}
	return time.Time{}, false
}

// Set pauses the provider's peak hours until the given moment; the zero time
// ends the pause. Pauses that have run out are dropped on the way, and the file
// goes when none is left.
func (p *PeakPauses) Set(provider string, until, now time.Time) error {
	if p.path == "" {
		return errors.New("no directory for the pause file")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	raw := map[string]string{}
	for name, t := range p.read() {
		if t.After(now) {
			raw[name] = t.Format(time.RFC3339)
		}
	}
	if until.IsZero() || !until.After(now) {
		delete(raw, provider)
	} else {
		raw[provider] = until.Format(time.RFC3339)
	}
	if len(raw) == 0 {
		if err := os.Remove(p.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	return ktavfile.Write(p.path, raw)
}
