package codex

import (
	"encoding/json"
	"hash/fnv"
	"strings"
)

// Claude Code sends background requests (a suggestion of what to type next, for
// one) that carry the conversation's history, with the same system prompt and
// tools, plus a short fixed user message. Answered on the conversation's
// websocket they become the connection's latest response, and the backend keeps
// no other with store=false: the conversation's next request, which does not
// hold that message or its reply, can no longer continue the chain and goes out
// in full (2-4 MiB). So the manager learns such a message, from the one
// divergence that shows it, and requests that end in it skip the websocket.
//
// A message taken for a side one by mistake (an ordinary turn) shows itself in
// the bucket's next request, which still holds it, followed by its reply: it is
// forgotten and never learned again.

const (
	// wsSideMaxSize bounds a learned message: a template, not a user's text.
	wsSideMaxSize = 8 << 10
	// wsSideMax bounds the messages remembered (learned, and found wrong).
	wsSideMax = 64
	// wsSideEnd is how much of each end of a message's text tells it apart: the
	// template is fixed and a part in its middle varies from request to request,
	// so the whole text would be learned again every time.
	wsSideEnd = 64
	// wsSideChecks bounds the buckets with a side request waiting for its check.
	wsSideChecks = 256
)

// wsSideTemplate is a learned side message: its size when learned and the
// requests it sent over HTTP.
type wsSideTemplate struct {
	size    int
	matches int
}

// wsSideCheck is a request of a bucket sent over HTTP as a side one: its last
// item, at index at, must not be in the bucket's next longer request.
type wsSideCheck struct {
	key  uint64
	at   int
	item uint64
}

// sideText is the text of a user message from its encoding; false for any
// other item.
func sideText(encoded []byte) (string, bool) {
	var item struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(encoded, &item) != nil || item.Type != "message" || item.Role != "user" || len(item.Content) == 0 {
		return "", false
	}
	var b strings.Builder
	for _, part := range item.Content {
		b.WriteString(part.Text)
	}
	return b.String(), true
}

// sideKey identifies a side message by both ends of its text; a short text
// by the whole of it.
func sideKey(text string) uint64 {
	h := fnv.New64a()
	if len(text) <= 2*wsSideEnd {
		_, _ = h.Write([]byte(text))
	} else {
		_, _ = h.Write([]byte(text[:wsSideEnd]))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(text[len(text)-wsSideEnd:]))
	}
	return h.Sum64()
}

func hashBytes(b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}

// lastItemSide is the side key of the last input item, the hash and size of
// its encoding; false without a cache key, or when the last item is not a user
// message of at most wsSideMaxSize.
func lastItemSide(req *Request) (key, item uint64, size int, ok bool) {
	if req == nil || req.PromptCacheKey == "" || len(req.Input) == 0 {
		return 0, 0, 0, false
	}
	encoded, err := json.Marshal(req.Input[len(req.Input)-1])
	if err != nil || len(encoded) > wsSideMaxSize {
		return 0, 0, 0, false
	}
	text, ok := sideText(encoded)
	if !ok {
		return 0, 0, 0, false
	}
	return sideKey(text), hashBytes(encoded), len(encoded), true
}

// WSSide describes a message learned as a side request's.
type WSSide struct {
	Size int
}

// WSSideWrong describes a learned message found to be an ordinary turn's.
type WSSideWrong struct {
	Size    int // when learned
	Matches int // requests it sent over HTTP
}

// learnSide remembers the last item of the chain as a side message when the
// divergence d shows the pattern: the chain's last item is a small user message
// and the reply to it is not in the request that follows. Called without the lock.
func (m *WSManager) learnSide(chain *WSChain, d *WSDivergence) {
	if chain == nil || d == nil || !d.Reply || d.ReplyAt != -1 || d.At != d.Had-1 || d.At >= len(chain.hashes) ||
		len(chain.last) == 0 || d.Was.Kind != "user message" || d.Was.Size > wsSideMaxSize {
		return
	}
	text, ok := sideText(chain.last)
	if !ok {
		return
	}
	key := sideKey(text)
	m.mu.Lock()
	_, known := m.side[key]
	_, wrong := m.notSide[key]
	if known || wrong {
		m.mu.Unlock()
		return
	}
	if m.side == nil || len(m.side) >= wsSideMax {
		m.side = make(map[uint64]*wsSideTemplate)
	}
	m.side[key] = &wsSideTemplate{size: d.Was.Size}
	notify := m.OnSide
	m.mu.Unlock()
	if notify != nil {
		notify(WSSide{Size: d.Was.Size})
	}
}

// SideRequest says whether req, a request of bucket, ends in a message learned
// as a side request's and should go over HTTP so that it does not replace the
// websocket's chain; a true answer is counted. wrong is set when req shows that
// the bucket's last such request was an ordinary turn: its message is
// forgotten.
func (m *WSManager) SideRequest(bucket string, req *Request) (side bool, wrong *WSSideWrong) {
	if req == nil {
		return false, nil
	}
	m.mu.Lock()
	check, pending := m.sideSent[bucket]
	m.mu.Unlock()
	// The check needs the item's encoding, made without the lock. The message
	// must be followed by something (its reply): a request ending in it is
	// another side request.
	var at uint64
	checked := pending && len(req.Input) > check.at+1
	if checked {
		if encoded, err := json.Marshal(req.Input[check.at]); err == nil {
			at = hashBytes(encoded)
		}
	}
	key, item, _, ok := lastItemSide(req)

	m.mu.Lock()
	defer m.mu.Unlock()
	if checked && m.sideSent[bucket] == check {
		delete(m.sideSent, bucket)
		if at == check.item {
			if t, learned := m.side[check.key]; learned {
				delete(m.side, check.key)
				if m.notSide == nil || len(m.notSide) >= wsSideMax {
					m.notSide = make(map[uint64]struct{})
				}
				m.notSide[check.key] = struct{}{}
				m.stats.SideWrong++
				wrong = &WSSideWrong{Size: t.size, Matches: t.matches}
			}
		}
	}
	if !ok {
		return false, wrong
	}
	t, learned := m.side[key]
	if !learned {
		return false, wrong
	}
	t.matches++
	m.stats.Side++
	if m.sideSent == nil || len(m.sideSent) >= wsSideChecks {
		m.sideSent = make(map[string]wsSideCheck)
	}
	m.sideSent[bucket] = wsSideCheck{key: key, at: len(req.Input) - 1, item: item}
	return true, wrong
}
