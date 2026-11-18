package obs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/sse"
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
type CacheWatch struct {
	Mu         sync.Mutex
	Last       map[string]watched
	Active     map[string]map[uint64]struct{}
	order      []string // keys, oldest first
	generation uint64
}

var PromptWatch = &CacheWatch{}

const (
	MaxTurnStateBuckets = 512
	TurnStateTTL        = 30 * time.Minute
)

type TurnStateKey struct {
	key    string
	system string
	tools  string
}

type turnStateBucket struct {
	value      string
	expiresAt  time.Time
	generation uint64
	sequence   uint64
}

type TurnStateStore struct {
	mu         sync.Mutex
	Buckets    map[TurnStateKey]*turnStateBucket
	order      []TurnStateKey
	generation uint64
	sequence   uint64
}

var TurnStates = &TurnStateStore{}

type turnRegistration struct {
	ThreadID   string
	State      string
	NewTurn    bool
	store      *TurnStateStore
	key        TurnStateKey
	generation uint64
	sequence   uint64
}

func (s *TurnStateStore) Begin(f codex.Fingerprint, continuation, newTurn bool, now time.Time) turnRegistration {
	key := TurnStateKey{key: f.Key, system: f.System, tools: f.Tools}
	reg := turnRegistration{ThreadID: TurnThreadID(key), NewTurn: newTurn}
	if f.Key == "" {
		return reg
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Buckets == nil {
		s.Buckets = make(map[TurnStateKey]*turnStateBucket)
	}
	s.pruneExpiredLocked(now)
	bucket, ok := s.Buckets[key]
	if !ok {
		if len(s.Buckets) == MaxTurnStateBuckets {
			oldest := s.order[0]
			s.order = s.order[1:]
			delete(s.Buckets, oldest)
		}
		s.generation++
		if s.generation == 0 {
			s.generation++
		}
		bucket = &turnStateBucket{generation: s.generation, expiresAt: now.Add(TurnStateTTL)}
		s.Buckets[key] = bucket
		s.order = append(s.order, key)
	}
	if newTurn {
		bucket.value = ""
		bucket.expiresAt = now.Add(TurnStateTTL)
	}
	s.sequence++
	if s.sequence == 0 {
		s.sequence++
	}
	bucket.sequence = s.sequence
	reg.store = s
	reg.key = key
	reg.generation = bucket.generation
	reg.sequence = bucket.sequence
	if continuation {
		reg.State = bucket.value
	}
	return reg
}

func (r turnRegistration) Store(value string, now time.Time) {
	if r.store == nil {
		return
	}
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.Buckets[r.key]
	if !ok || bucket.generation != r.generation || bucket.sequence != r.sequence {
		return
	}
	if !bucket.expiresAt.IsZero() && !now.Before(bucket.expiresAt) {
		s.removeLocked(r.key)
		return
	}
	bucket.value = value
	bucket.expiresAt = now.Add(TurnStateTTL)
}

func (s *TurnStateStore) pruneExpiredLocked(now time.Time) {
	if len(s.Buckets) == 0 {
		return
	}
	order := s.order[:0]
	for _, key := range s.order {
		bucket, ok := s.Buckets[key]
		if ok && (bucket.expiresAt.IsZero() || now.Before(bucket.expiresAt)) {
			order = append(order, key)
		} else {
			delete(s.Buckets, key)
		}
	}
	s.order = order
}

func (s *TurnStateStore) removeLocked(key TurnStateKey) {
	delete(s.Buckets, key)
	for i, ordered := range s.order {
		if ordered == key {
			copy(s.order[i:], s.order[i+1:])
			s.order = s.order[:len(s.order)-1]
			break
		}
	}
}

func TurnThreadID(key TurnStateKey) string {
	encoded, _ := json.Marshal([3]string{key.key, key.system, key.tools})
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

type prevInfo struct {
	start, ended time.Time
	Edge         string
}

type cacheRegistration struct {
	text       string
	Prev       prevInfo
	Generation uint64
	running    int
	branch     bool
}

// See atomically registers f and returns the comparison with its predecessor,
// the predecessor's timing and edge, and an identity for this request.
func (w *CacheWatch) See(f codex.Fingerprint, items []uint64) cacheRegistration {
	if f.Key == "" {
		return cacheRegistration{}
	}
	w.Mu.Lock()
	defer w.Mu.Unlock()
	if w.Last == nil {
		w.Last = map[string]watched{}
	}
	prev, seen := w.Last[f.Key]
	active := w.Active[f.Key]
	reg := cacheRegistration{
		Prev:       prevInfo{start: prev.start, ended: prev.ended, Edge: prev.edge},
		running:    len(active),
		Generation: 0,
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
			delete(w.Last, w.order[0])
			w.order = w.order[1:]
		}
	}
	w.generation++
	if w.generation == 0 {
		w.generation++
	}
	current := watched{f: f, items: items, start: time.Now(), generation: w.generation}
	w.Last[f.Key] = current
	if w.Active == nil {
		w.Active = map[string]map[uint64]struct{}{}
	}
	if w.Active[f.Key] == nil {
		w.Active[f.Key] = map[uint64]struct{}{}
	}
	w.Active[f.Key][current.generation] = struct{}{}

	reg.Generation = current.generation
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
func (w *CacheWatch) Done(key string, generation uint64, input int, edge string) {
	if key == "" || generation == 0 {
		return
	}
	w.Mu.Lock()
	defer w.Mu.Unlock()
	w.removeActiveLocked(key, generation)
	if input <= 0 && edge == "" {
		return
	}
	if cur, ok := w.Last[key]; ok && cur.generation == generation {
		if input > 0 {
			cur.input = input
		}
		if edge != "" {
			cur.edge = edge
		}
		cur.ended = time.Now()
		w.Last[key] = cur
	}
}

// Finish ends a request without recording diagnostic details.
func (w *CacheWatch) Finish(key string, generation uint64) {
	if key == "" || generation == 0 {
		return
	}
	w.Mu.Lock()
	defer w.Mu.Unlock()
	w.removeActiveLocked(key, generation)
	if cur, ok := w.Last[key]; ok && cur.generation == generation && cur.ended.IsZero() {
		cur.ended = time.Now()
		w.Last[key] = cur
	}
}

func (w *CacheWatch) removeActiveLocked(key string, generation uint64) {
	active := w.Active[key]
	delete(active, generation)
	if len(active) == 0 {
		delete(w.Active, key)
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
func PromptText(f codex.Fingerprint, reg cacheRegistration, now time.Time) string {
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
	if t := timingText(reg.Prev, now); t != "" {
		text += "; " + t
	}
	return text
}

// Pending counts the prompt keys still being watched and the entries kept, and
// names the entries that have no end time yet.
func (w *CacheWatch) Pending() (active, entries int, unended []string) {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	for key, cur := range w.Last {
		if cur.ended.IsZero() {
			unended = append(unended, key)
		}
	}
	return len(w.Active), len(w.Last), unended
}

// TurnKey is the turn-state key of a request fingerprint.
func TurnKey(f codex.Fingerprint) TurnStateKey {
	return TurnStateKey{key: f.Key, system: f.System, tools: f.Tools}
}

// Groups a big prompt is counted in besides the total: how long before it the
// last request of its key started (a cache entry is kept for a few minutes) and
// whether that request's reply came through the same data centre.
const (
	groupGapShort         = "started <5m before"
	groupGapMiddle        = "started 5-10m before"
	groupGapLong          = "started >10m before"
	groupEdgeSame         = "same edge"
	groupEdgeOther        = "other edge"
	groupTurnStateSent    = "turn state sent"
	groupTurnStateNotSent = "turn state not sent"
)

// cacheGroups is the order the groups are printed in.
var cacheGroups = []string{groupGapShort, groupGapMiddle, groupGapLong, groupEdgeSame, groupEdgeOther, groupTurnStateSent, groupTurnStateNotSent}

type cacheCount struct {
	n             int
	input, cached int64
}

// cacheStats counts how much of the big prompts the backend read from its cache
// since the last Take; it covers every request, not only the slow ones that
// get a line of their own.
type CacheStats struct {
	mu                sync.Mutex
	requests, partial int
	miss              int
	input, cached     int64
	groups            map[string]*cacheCount
}

var PromptStats = &CacheStats{}

// Add counts the usage of one finished reply, in the total and in each of the
// groups ("" is skipped: nothing was known); prompts under cacheMissMin are
// never cached anyway and not counted.
func (s *CacheStats) Add(us sse.Usage, groups ...string) {
	if us.Input < cacheMissMin {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	s.input += int64(us.Input)
	s.cached += int64(us.Cached)
	switch {
	case us.Cached == 0:
		s.miss++
	case us.Cached*100 < us.Input*cachePartialPct:
		s.partial++
	}
	for _, g := range groups {
		if g == "" {
			continue
		}
		if s.groups == nil {
			s.groups = map[string]*cacheCount{}
		}
		c := s.groups[g]
		if c == nil {
			c = &cacheCount{}
			s.groups[g] = c
		}
		c.n++
		c.input += int64(us.Input)
		c.cached += int64(us.Cached)
	}
}

// Take is the counts as two log texts, the total and the groups, and starts them
// again; both "" when nothing was counted.
func (s *CacheStats) Take() (total, by string) {
	s.mu.Lock()
	r, p, m, in, c, groups := s.requests, s.partial, s.miss, s.input, s.cached, s.groups
	s.requests, s.partial, s.miss, s.input, s.cached, s.groups = 0, 0, 0, 0, 0, nil
	s.mu.Unlock()
	if r == 0 {
		return "", ""
	}
	total = fmt.Sprintf("%d big prompts, %d read from the cache in part (under %d%%), %d not at all; %d%% of %s input tokens came from the cache",
		r, p, cachePartialPct, m, c*100/in, millions(in))
	var parts []string
	for _, g := range cacheGroups {
		if gc := groups[g]; gc != nil {
			parts = append(parts, fmt.Sprintf("%s: %d prompts, %d%% cached", g, gc.n, gc.cached*100/gc.input))
		}
	}
	return total, strings.Join(parts, "; ")
}

// gapGroup puts a request by how long after the start of the key's last one it
// started; "" when there was none.
func GapGroup(prev prevInfo, start time.Time) string {
	if prev.start.IsZero() {
		return ""
	}
	switch d := start.Sub(prev.start); {
	case d < 5*time.Minute:
		return groupGapShort
	case d < 10*time.Minute:
		return groupGapMiddle
	}
	return groupGapLong
}

// edgeGroup tells whether the reply came through the data centre of the key's
// last one; "" when either is unknown.
func EdgeGroup(prev, cur string) string {
	switch {
	case prev == "" || cur == "":
		return ""
	case prev == cur:
		return groupEdgeSame
	}
	return groupEdgeOther
}

// turnStateGroup keeps the sent/not-sent split in stats.
func TurnStateGroup(sent bool) string {
	if sent {
		return groupTurnStateSent
	}
	return groupTurnStateNotSent
}

// millions writes n tokens as 23.4M, or 812k under a million.
func millions(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
	return fmt.Sprintf("%dk", n/1000)
}

// WriteCacheStats prints the cache lines of the Codex prompts since the last call.
func WriteCacheStats(w io.Writer, now time.Time) {
	total, by := PromptStats.Take()
	if total == "" {
		return
	}
	stamp := now.Format("15:04:05")
	_, _ = fmt.Fprintf(w, "[%s] upstream cache [codex]: %s\n", stamp, total)
	if by != "" {
		_, _ = fmt.Fprintf(w, "[%s] upstream cache by [codex]: %s\n", stamp, by)
	}
}

// edgeOf is the data centre in the tail of a Cf-Ray header (a56de7373f3d69dc-DFW
// gives DFW); "" when there is none.
func EdgeOf(h http.Header) string {
	ray := h.Get("Cf-Ray")
	i := strings.LastIndexByte(ray, '-')
	if i < 0 || i == len(ray)-1 {
		return ""
	}
	return ray[i+1:]
}

// edgeText says whether a reply came through the same data centre as the last
// request of its key (a cache may live per region): "" when either is unknown.
func edgeText(prev, cur string) string {
	switch {
	case prev == "" || cur == "":
		return ""
	case prev == cur:
		return "edge same (" + cur + ")"
	}
	return "edge " + prev + " -> " + cur
}
