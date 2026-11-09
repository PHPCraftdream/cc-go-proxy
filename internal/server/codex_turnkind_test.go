package server

import (
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

func TestCodexTurnKind(t *testing.T) {
	user := map[string]any{"type": "message", "role": "user"}
	assistant := map[string]any{"type": "message", "role": "assistant"}
	call := map[string]any{"type": "function_call"}
	output := map[string]any{"type": "function_call_output"}
	reasoning := map[string]any{"type": "reasoning"}

	for _, tc := range []struct {
		name         string
		input        []any
		continuation bool
		newTurn      bool
	}{
		{"empty", nil, false, false},
		{"first user prompt", []any{user}, false, true},
		{"new prompt after an answer", []any{user, assistant, user}, false, true},
		{"new prompt after a tool loop", []any{user, call, output, assistant, user}, false, true},
		{"tool output last", []any{user, call, output}, true, false},
		{"tool output then reminder text (one converted user message)", []any{user, call, output, user}, true, false},
		{"several outputs then text", []any{user, call, call, output, output, user}, true, false},
		{"ends with an assistant call", []any{user, call}, false, false},
		{"reasoning before the user prompt", []any{user, reasoning, assistant, user}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			continuation, newTurn := codexTurnKind(&codex.Request{Input: tc.input})
			if continuation != tc.continuation || newTurn != tc.newTurn {
				t.Errorf("codexTurnKind = (continuation %v, new turn %v), want (%v, %v)", continuation, newTurn, tc.continuation, tc.newTurn)
			}
		})
	}
	if c, n := codexTurnKind(nil); c || n {
		t.Errorf("nil request = (%v, %v), want (false, false)", c, n)
	}
}
