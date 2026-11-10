package codex

import "fmt"

// AgentKey tells the agents of one conversation apart. Claude Code's subagents
// share the conversation's cache key, system prompt and tools, so those cannot
// name a websocket; what differs is the first user message (the agent's task),
// and it does not change while the agent runs, which a delta needs anyway.
// "" when the request holds no user message.
func AgentKey(req *Request) string {
	if req == nil {
		return ""
	}
	for _, item := range req.Input {
		if itemShape(item).Kind != "user message" {
			continue
		}
		hashes := ItemHashes(&Request{PromptCacheKey: "agent", Input: []any{item}})
		return fmt.Sprintf("%08x", hashes[0]&0xffffffff)
	}
	return ""
}
