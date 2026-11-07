package server

import (
	"fmt"
	"sync"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

// maxWatchedKeys bounds what cacheWatch remembers.
const maxWatchedKeys = 512

// watched is what cacheWatch keeps of a request: its fingerprint and the hash of
// every input item.
type watched struct {
	f     codex.Fingerprint
	items []uint64
}

// cacheWatch remembers the last request of every cache key (a Claude Code
// session), so a line of the log can say whether the prompt changed since: a
// cache miss then has a cause to read off, not to guess.
type cacheWatch struct {
	mu    sync.Mutex
	last  map[string]watched
	order []string // keys, oldest first
}

var promptWatch = &cacheWatch{}

// See records f (items: the hashes of its input items, nil when unknown) and
// says how it differs from the last request of its key: "" for a request
// without a key.
func (w *cacheWatch) See(f codex.Fingerprint, items []uint64) string {
	if f.Key == "" {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		w.last = map[string]watched{}
	}
	prev, seen := w.last[f.Key]
	if !seen {
		w.order = append(w.order, f.Key)
		if len(w.order) > maxWatchedKeys {
			delete(w.last, w.order[0])
			w.order = w.order[1:]
		}
	}
	w.last[f.Key] = watched{f, items}
	if !seen {
		return "first request of this key since the server started"
	}
	var changed []string
	if prev.f.System != f.System {
		changed = append(changed, "system prompt")
	}
	if prev.f.Tools != f.Tools {
		changed = append(changed, "tools")
	}
	text := "same system prompt and tools as the last request of this key"
	switch len(changed) {
	case 1:
		text = changed[0] + " changed since the last request of this key"
	case 2:
		text = "system prompt and tools changed since the last request of this key"
	}
	if f.Items < prev.f.Items {
		text += fmt.Sprintf("; history shorter (%d -> %d items)", prev.f.Items, f.Items)
	}
	if note := historyNote(prev.items, items); note != "" {
		text += "; " + note
	}
	return text
}

// historyNote compares the input items with the last request's: where the first
// one that differs is (a cache is read by prefix, so the items before it still
// hit), or that the new history only extends the old one. "" when either side
// has no hashes or a shorter history only lost its tail.
func historyNote(prev, cur []uint64) string {
	if prev == nil || cur == nil {
		return ""
	}
	n := min(len(prev), len(cur))
	for i := range n {
		if prev[i] != cur[i] {
			return fmt.Sprintf("history changed at item %d of %d (the last request had %d)", i, len(cur), len(prev))
		}
	}
	switch {
	case len(cur) > len(prev):
		return fmt.Sprintf("history extends the last request by %d items", len(cur)-len(prev))
	case len(cur) == len(prev):
		return "history identical to the last request"
	}
	return ""
}

// promptText is what a log line says about the prompt of a request.
func promptText(f codex.Fingerprint, items []uint64) string {
	text := f.String()
	if note := promptWatch.See(f, items); note != "" {
		text += "; " + note
	}
	return text
}
