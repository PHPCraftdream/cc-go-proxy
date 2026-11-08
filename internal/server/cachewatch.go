package server

import (
	"fmt"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

// maxWatchedKeys bounds what cacheWatch remembers.
const maxWatchedKeys = 512

// watched is what cacheWatch keeps of a request: its fingerprint, the hash of
// every input item and, once the reply is over, the input tokens it was billed.
type watched struct {
	f     codex.Fingerprint
	items []uint64
	input int    // 0 until Done
	edge  string // data centre of the reply (Cf-Ray), "" until Done
	start time.Time
	ended time.Time // zero until Done
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
	w.last[f.Key] = watched{f: f, items: items, start: time.Now()}
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
	if prev.input > 0 {
		text += fmt.Sprintf("; the last request had %d tokens in", prev.input)
	}
	return text
}

// Done records the input tokens and the data centre (edge) of the reply to the
// request seen as f, so the next request of its key can say what the last one
// was billed (a `cached` below it means the backend dropped part of what it
// had) and where it came from. A newer request of the key that was seen
// meanwhile is left alone.
func (w *cacheWatch) Done(f codex.Fingerprint, input int, edge string) {
	if f.Key == "" || (input <= 0 && edge == "") {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if cur, ok := w.last[f.Key]; ok && cur.f == f {
		if input > 0 {
			cur.input = input
		}
		if edge != "" {
			cur.edge = edge
		}
		cur.ended = time.Now()
		w.last[f.Key] = cur
	}
}

// prevInfo is what is known of the last request of a key when a new one comes:
// when it started and ended and the data centre of its reply. Zero when unknown.
type prevInfo struct {
	start, ended time.Time
	edge         string
}

// Prev says what is known of the last request of key.
func (w *cacheWatch) Prev(key string) prevInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.last[key]
	return prevInfo{start: p.start, ended: p.ended, edge: p.edge}
}

// timingText says how long before now the last request of the key started and
// ended: a cache entry is kept for a few minutes, so a long stream before this
// request may be why the cache is gone. "" when there was no last request.
func timingText(p prevInfo, now time.Time) string {
	if p.start.IsZero() {
		return ""
	}
	since := now.Sub(p.start).Round(time.Second)
	if p.ended.IsZero() {
		return fmt.Sprintf("the last request started %s ago and had not ended", since)
	}
	return fmt.Sprintf("the last request started %s ago, ended %s ago", since, now.Sub(p.ended).Round(time.Second))
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
	prev := promptWatch.Prev(f.Key)
	if note := promptWatch.See(f, items); note != "" {
		text += "; " + note
	}
	if t := timingText(prev, time.Now()); t != "" {
		text += "; " + t
	}
	return text
}
