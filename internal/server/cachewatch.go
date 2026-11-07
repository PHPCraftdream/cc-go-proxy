package server

import (
	"fmt"
	"sync"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

// maxWatchedKeys bounds what cacheWatch remembers.
const maxWatchedKeys = 512

// cacheWatch remembers the fingerprint of the last request of every cache key
// (a Claude Code session), so a line of the log can say whether the prompt
// changed since: a cache miss then has a cause to read off, not to guess.
type cacheWatch struct {
	mu    sync.Mutex
	last  map[string]codex.Fingerprint
	order []string // keys, oldest first
}

var promptWatch = &cacheWatch{}

// See records f and says how it differs from the last request of its key: ""
// for a request without a key.
func (w *cacheWatch) See(f codex.Fingerprint) string {
	if f.Key == "" {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		w.last = map[string]codex.Fingerprint{}
	}
	prev, seen := w.last[f.Key]
	if !seen {
		w.order = append(w.order, f.Key)
		if len(w.order) > maxWatchedKeys {
			delete(w.last, w.order[0])
			w.order = w.order[1:]
		}
	}
	w.last[f.Key] = f
	if !seen {
		return "first request of this key since the server started"
	}
	var changed []string
	if prev.System != f.System {
		changed = append(changed, "system prompt")
	}
	if prev.Tools != f.Tools {
		changed = append(changed, "tools")
	}
	text := "same system prompt and tools as the last request of this key"
	switch len(changed) {
	case 1:
		text = changed[0] + " changed since the last request of this key"
	case 2:
		text = "system prompt and tools changed since the last request of this key"
	}
	if f.Items < prev.Items {
		text += fmt.Sprintf("; history shorter (%d -> %d items)", prev.Items, f.Items)
	}
	return text
}

// promptText is what a log line says about the prompt of a request.
func promptText(f codex.Fingerprint) string {
	text := f.String()
	if note := promptWatch.See(f); note != "" {
		text += "; " + note
	}
	return text
}
