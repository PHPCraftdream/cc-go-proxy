package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
)

// Fingerprint describes the part of a request the backend caches by its prefix:
// the cache key, the system prompt and the tools. It is for the log, to tell why
// two requests of one session did not share a cache; it holds hashes and counts,
// never a word of the prompt or the id the key comes from.
type Fingerprint struct {
	Key    string // first 8 hex digits of prompt_cache_key; "" when the request has none
	System string // 8 hex digits of the system prompt; "" when there is none
	Tools  string // 8 hex digits of the tool definitions; "" when there are none
	NTools int
	Items  int // input items: the system prompt and the whole history
}

// NewFingerprint takes the fingerprint of a converted request.
func NewFingerprint(req *Request) Fingerprint {
	f := Fingerprint{NTools: len(req.Tools), Items: len(req.Input)}
	if len(req.PromptCacheKey) >= 8 {
		f.Key = req.PromptCacheKey[:8]
	}
	if len(req.Input) > 0 {
		if m, ok := req.Input[0].(messageItem); ok && m.Role == "developer" {
			f.System = hash8(m)
		}
	}
	if len(req.Tools) > 0 {
		f.Tools = hash8(req.Tools)
	}
	return f
}

// ItemHashes hashes every input item in order. The log compares them with the
// last request of the same session to name the first item that changed.
func ItemHashes(req *Request) []uint64 {
	out := make([]uint64, len(req.Input))
	h := fnv.New64a()
	for i, it := range req.Input {
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		h.Reset()
		_, _ = h.Write(b)
		out[i] = h.Sum64()
	}
	return out
}

func hash8(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:4])
}

// String is the fingerprint as a log line says it.
func (f Fingerprint) String() string {
	key, system, tools := "no cache key", "no system prompt", "no tools"
	if f.Key != "" {
		key = "key " + f.Key
	}
	if f.System != "" {
		system = "system " + f.System
	}
	if f.Tools != "" {
		tools = fmt.Sprintf("%d tools %s", f.NTools, f.Tools)
	}
	return fmt.Sprintf("%s, %s, %s, %d input items", key, system, tools, f.Items)
}
