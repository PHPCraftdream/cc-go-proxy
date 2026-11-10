package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
)

// Fingerprint describes the part of a request the backend caches by its prefix:
// the model, cache key, system prompt, tools and request controls. It is for the
// log, to tell why two requests of one session did not share a cache; it holds
// hashes and counts, never control contents or the id the key comes from.
type Fingerprint struct {
	Key        string // prompt_cache_key; "" when the request has none
	Model      string // backend model; "" when the request has none
	System     string // 8 hex digits of the system prompt; "" when there is none
	Tools      string // 8 hex digits of the tool definitions; "" when there are none
	Reasoning  string // 8 hex digits of the reasoning settings; "" when there are none
	ToolChoice string // 8 hex digits of tool_choice; "" when there is none
	NTools     int
	Items      int // input items: the system prompt and the whole history
}

// NewFingerprint takes the fingerprint of a converted request.
func NewFingerprint(req *Request) Fingerprint {
	f := Fingerprint{NTools: len(req.Tools), Items: len(req.Input)}
	f.Key = req.PromptCacheKey
	f.Model = req.Model
	if len(req.Input) > 0 {
		if m, ok := req.Input[0].(messageItem); ok && m.Role == "developer" {
			f.System = hash8(m)
		}
	}
	if len(req.Tools) > 0 {
		f.Tools = hash8(req.Tools)
	}
	if req.Reasoning != nil {
		f.Reasoning = hash8(req.Reasoning)
	}
	if req.ToolChoice != nil {
		f.ToolChoice = hash8(req.ToolChoice)
	}
	return f
}

// ItemHashes hashes every input item in order. The log compares them with the
// last request of the same session to name the first item that changed.
func ItemHashes(req *Request) []uint64 {
	hashes, _, _ := itemDigests(req, false)
	return hashes
}

// itemDigests hashes every input item and, with sized, also returns the encoded
// size of each and a copy of the last item's encoding.
func itemDigests(req *Request, sized bool) (hashes []uint64, sizes []int, last []byte) {
	if req.PromptCacheKey == "" {
		return nil, nil, nil
	}

	hashes = make([]uint64, len(req.Input))
	if sized {
		sizes = make([]int, len(req.Input))
	}
	h := fnv.New64a()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i, it := range req.Input {
		buf.Reset()
		if err := enc.Encode(it); err != nil {
			continue
		}
		b := buf.Bytes()[:buf.Len()-1] // Encoder appends a newline; Marshal does not.
		h.Reset()
		_, _ = h.Write(b)
		hashes[i] = h.Sum64()
		if sized {
			sizes[i] = len(b)
			if i == len(req.Input)-1 {
				last = append([]byte(nil), b...)
			}
		}
	}
	return hashes, sizes, last
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
		key = "key " + f.Key[:min(8, len(f.Key))]
	}
	if f.System != "" {
		system = "system " + f.System
	}
	if f.Tools != "" {
		tools = fmt.Sprintf("%d tools %s", f.NTools, f.Tools)
	}
	return fmt.Sprintf("%s, %s, %s, %d input items", key, system, tools, f.Items)
}
