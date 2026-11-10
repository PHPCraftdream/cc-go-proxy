package codex

import (
	"testing"
	"time"
)

func TestAgentKeyFollowsTheFirstUserMessage(t *testing.T) {
	system := messageItem{Type: "message", Role: "developer", Content: []contentPart{{Type: "input_text", Text: "system"}}}
	task := wsManagerUser("task of agent A")
	grown := makeWSManagerTestRequest("k", system, task, wsSideAssistant("a1"), wsManagerUser("more"), functionOutputItem{Type: "function_call_output", CallID: "c", Output: "x"})
	first := makeWSManagerTestRequest("k", system, task)

	key := AgentKey(first)
	if len(key) != 8 {
		t.Fatalf("AgentKey = %q, want 8 hex digits", key)
	}
	if AgentKey(grown) != key {
		t.Error("the key changed while the agent's history grew")
	}
	if AgentKey(makeWSManagerTestRequest("other-key", task)) != key {
		t.Error("the key depends on the cache key; the thread already carries that")
	}
	if AgentKey(makeWSManagerTestRequest("k", system, wsManagerUser("task of agent B"))) == key {
		t.Error("two agents with different tasks got one key")
	}
	if AgentKey(makeWSManagerTestRequest("k", system, wsSideAssistant("a1"), task)) != key {
		t.Error("an item before the first user message changed the key")
	}
}

func TestAgentKeyIsEmptyWithoutAUserMessage(t *testing.T) {
	if AgentKey(nil) != "" || AgentKey(makeWSManagerTestRequest("k")) != "" {
		t.Error("a nil or empty request has no agent key")
	}
	if got := AgentKey(makeWSManagerTestRequest("k", wsSideAssistant("a"), functionCallItem{Type: "function_call", CallID: "c"})); got != "" {
		t.Errorf("AgentKey without a user message = %q", got)
	}
	// Items that came back from JSON (maps) are recognised too.
	if got := AgentKey(makeWSManagerTestRequest("k", map[string]any{"type": "message", "role": "user", "content": []any{}})); got == "" {
		t.Error("a user message given as a map was not recognised")
	}
}

func TestWSManagerReportsEveryBreakWithItsAge(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: 25 * time.Minute, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	var got []WSLost
	m.OnLost = func(l WSLost) { got = append(got, l) }
	m.observeLost("owner", 12*time.Minute, 1001)
	m.observeLost("owner", 2*time.Minute, -1)
	m.observeLost("owner", 25*time.Minute, 1006)
	want := []WSLost{
		{Age: 12 * time.Minute, Status: 1001, Planned: 25 * time.Minute, Counts: true},
		{Age: 2 * time.Minute, Status: -1, Planned: 25 * time.Minute, Counts: false},
		{Age: 25 * time.Minute, Status: 1006, Planned: 25 * time.Minute, Counts: false},
	}
	if len(got) != len(want) {
		t.Fatalf("reports = %+v, want %d", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("report %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if m.TakeStats().Lost != 3 {
		t.Error("the breaks were not counted")
	}
}
