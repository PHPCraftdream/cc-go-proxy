package codex

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
)

// lastItemHash is the hash ItemHashes gives the last input item, false without one.
func lastItemHash(req *Request) (uint64, bool) {
	if req == nil || req.PromptCacheKey == "" || len(req.Input) == 0 {
		return 0, false
	}
	hashes := ItemHashes(&Request{PromptCacheKey: req.PromptCacheKey, Input: req.Input[len(req.Input)-1:]})
	return hashes[0], true
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
		d.Was.Kind != "user message" || d.Was.Size > wsSideMaxSize {
		return
	}
	hash := chain.hashes[d.At]
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
	hash, ok := lastItemHash(req)
	if !ok {
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
