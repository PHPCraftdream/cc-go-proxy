package codex

import (
	"encoding/json"
	"hash/fnv"
)

// Claude Code sends background requests (a suggestion of what to type next, for
// one) that carry the conversation's history, with the same system prompt and
// tools, plus a short fixed user message. Answered on the conversation's
// websocket they become the connection's latest response, and the backend keeps
// no other with store=false: the conversation's next request, which does not
// hold that message or its reply, can no longer continue the chain and goes out
// in full (2-4 MiB). So the manager learns such a message, from the one
// divergence that shows it, and requests that end in it skip the websocket.

const (
	// wsSideMaxSize bounds a learned message: a template, not a user's text.
	wsSideMaxSize = 8 << 10
	// wsSideMax bounds the messages remembered.
	wsSideMax = 64
	// wsSideHead is how much of a message's encoding tells it apart: the template
	// is fixed and a part of it varies from request to request (the middle of
	// 700 bytes), so the whole message would be learned again every time.
	wsSideHead = 128
)

// sideKey identifies a side message by the start of its encoding.
func sideKey(encoded []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(encoded[:min(len(encoded), wsSideHead)])
	return h.Sum64()
}

// lastItemSideKey is the key of the last input item and the size of its
// encoding, false without a cache key, without items or without an encoding.
func lastItemSideKey(req *Request) (key uint64, size int, ok bool) {
	if req == nil || req.PromptCacheKey == "" || len(req.Input) == 0 {
		return 0, 0, false
	}
	encoded, err := json.Marshal(req.Input[len(req.Input)-1])
	if err != nil {
		return 0, 0, false
	}
	return sideKey(encoded), len(encoded), true
}

// WSSide describes a message learned as a side request's.
type WSSide struct {
	Size int
}

// learnSide remembers the last item of the chain as a side message when the
// divergence d shows the pattern: the chain's last item is a small user message
// and the reply to it is not in the request that follows. Called without the lock.
func (m *WSManager) learnSide(chain *WSChain, d *WSDivergence) {
	if chain == nil || d == nil || !d.Reply || d.ReplyAt != -1 || d.At != d.Had-1 || d.At >= len(chain.hashes) ||
		len(chain.last) == 0 || d.Was.Kind != "user message" || d.Was.Size > wsSideMaxSize {
		return
	}
	hash := sideKey(chain.last)
	m.mu.Lock()
	if _, known := m.side[hash]; known {
		m.mu.Unlock()
		return
	}
	if m.side == nil || len(m.side) >= wsSideMax {
		m.side = make(map[uint64]struct{})
	}
	m.side[hash] = struct{}{}
	notify := m.OnSide
	m.mu.Unlock()
	if notify != nil {
		notify(WSSide{Size: d.Was.Size})
	}
}

// SideRequest says whether req ends in a message learned as a side request's,
// which should go over HTTP so that it does not replace the websocket's chain.
// A true answer is counted.
func (m *WSManager) SideRequest(req *Request) bool {
	hash, size, ok := lastItemSideKey(req)
	if !ok || size > wsSideMaxSize {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, side := m.side[hash]; !side {
		return false
	}
	m.stats.Side++
	return true
}
