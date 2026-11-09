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
	f          codex.Fingerprint
	items      []uint64
	input      int    // 0 until Done
	edge       string // data centre of the reply (Cf-Ray), "" until Done
	start      time.Time
	ended      time.Time // zero until Done
	generation uint64
}

// cacheWatch remembers the last request of every cache key (a Claude Code
// session), so a line of the log can say whether the prompt changed since: a
// cache miss then has a cause to read off, not to guess.
type cacheWatch struct {
	mu         sync.Mutex
	last       map[string]watched
	active     map[string]map[uint64]struct{}
	order      []string // keys, oldest first
	generation uint64
}

var promptWatch = &cacheWatch{}

type prevInfo struct {
	start, ended time.Time
	edge         string
}

type cacheRegistration struct {
	text       string
	prev       prevInfo
	generation uint64
	running    int
	branch     bool
}

// See atomically registers f and returns the comparison with its predecessor,
// the predecessor's timing and edge, and an identity for this request.
func (w *cacheWatch) See(f codex.Fingerprint, items []uint64) cacheRegistration {
	if f.Key == "" {
		return cacheRegistration{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		w.last = map[string]watched{}
	}
	prev, seen := w.last[f.Key]
	active := w.active[f.Key]
	reg := cacheRegistration{
		prev:       prevInfo{start: prev.start, ended: prev.ended, edge: prev.edge},
		running:    len(active),
		generation: 0,
	}
	if seen {
		if _, running := active[prev.generation]; running && prev.items != nil && items != nil {
			for i := 0; i < min(len(prev.items), len(items)); i++ {
				if prev.items[i] != items[i] {
					reg.branch = true
					break
				}
			}
		}
	}
	if !seen {
		w.order = append(w.order, f.Key)
		if len(w.order) > maxWatchedKeys {
			delete(w.last, w.order[0])
			w.order = w.order[1:]
		}
	}
	w.generation++
	if w.generation == 0 {
		w.generation++
	}
	current := watched{f: f, items: items, start: time.Now(), generation: w.generation}
	w.last[f.Key] = current
	if w.active == nil {
		w.active = map[string]map[uint64]struct{}{}
	}
	if w.active[f.Key] == nil {
		w.active[f.Key] = map[uint64]struct{}{}
	}
	w.active[f.Key][current.generation] = struct{}{}

	reg.generation = current.generation
	if !seen {
		reg.text = "first request of this key since the server started"
		return reg
	}
	var changed []string
	if prev.f.System != f.System {
		changed = append(changed, "system prompt")
	}
	if prev.f.Tools != f.Tools {
		changed = append(changed, "tools")
	}
	reg.text = "same system prompt and tools as the last request of this key"
	switch len(changed) {
	case 1:
		reg.text = changed[0] + " changed since the last request of this key"
	case 2:
		reg.text = "system prompt and tools changed since the last request of this key"
	}
	if prev.f.Model != f.Model {
		reg.text += fmt.Sprintf("; model changed %s -> %s", prev.f.Model, f.Model)
	}
	if prev.f.Reasoning != f.Reasoning {
		reg.text += "; reasoning changed"
	}
	if prev.f.ToolChoice != f.ToolChoice {
		reg.text += "; tool choice changed"
	}
	if f.Items < prev.f.Items {
		reg.text += fmt.Sprintf("; history shorter (%d -> %d items)", prev.f.Items, f.Items)
	}
	if note := historyNote(prev.items, items); note != "" {
		reg.text += "; " + note
	}
	if prev.input > 0 {
		reg.text += fmt.Sprintf("; the last request had %d tokens in", prev.input)
	}
	return reg
}

// Done records the input tokens and the data centre (edge) of the reply to the
// registered request. A later request of the key is left alone.
func (w *cacheWatch) Done(key string, generation uint64, input int, edge string) {
	if key == "" || generation == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removeActiveLocked(key, generation)
	if input <= 0 && edge == "" {
		return
	}
	if cur, ok := w.last[key]; ok && cur.generation == generation {
		if input > 0 {
			cur.input = input
		}
		if edge != "" {
			cur.edge = edge
		}
		cur.ended = time.Now()
		w.last[key] = cur
	}
}

// Finish ends a request without recording diagnostic details.
func (w *cacheWatch) Finish(key string, generation uint64) {
	if key == "" || generation == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removeActiveLocked(key, generation)
	if cur, ok := w.last[key]; ok && cur.generation == generation && cur.ended.IsZero() {
		cur.ended = time.Now()
		w.last[key] = cur
	}
}

func (w *cacheWatch) removeActiveLocked(key string, generation uint64) {
	active := w.active[key]
	delete(active, generation)
	if len(active) == 0 {
		delete(w.active, key)
	}
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

// promptText formats one fingerprint and the result of registering its request.
func promptText(f codex.Fingerprint, reg cacheRegistration, now time.Time) string {
	text := f.String()
	if reg.text != "" {
		text += "; " + reg.text
	}
	if reg.running > 0 {
		unit := "requests"
		if reg.running == 1 {
			unit = "request"
		}
		text += fmt.Sprintf("; %d other %s of this key running", reg.running, unit)
	}
	if reg.branch {
		text += "; branch: history diverges from the last request while it is running"
	}
	if t := timingText(reg.prev, now); t != "" {
		text += "; " + t
	}
	return text
}
