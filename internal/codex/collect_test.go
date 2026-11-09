package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type collectorTestEvent struct {
	name string
	data any
}

func collectorStart(index int, block map[string]any) collectorTestEvent {
	return collectorTestEvent{"content_block_start", map[string]any{"index": index, "content_block": block}}
}

func collectorDelta(index int, delta map[string]any) collectorTestEvent {
	return collectorTestEvent{"content_block_delta", map[string]any{"index": index, "delta": delta}}
}

func emitCollectorEvents(c *Collector, events []collectorTestEvent) {
	for _, event := range events {
		c.Emit(event.name, event.data)
	}
}

func TestCollectorBuilderMatchesLegacyJSON(t *testing.T) {
	const repeatedDeltaCount = 2048
	textStart := map[string]any{"type": "text", "text": "Té"}
	thinkingStart := map[string]any{"type": "thinking", "thinking": "思", "signature": "old"}
	events := []collectorTestEvent{
		{"message_start", map[string]any{"message": map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "m",
			"stop_reason": nil, "usage": map[string]any{"input_tokens": 1, "output_tokens": 2},
		}}},
		collectorStart(0, textStart),
		collectorStart(1, thinkingStart),
		collectorStart(2, map[string]any{"type": "tool_use", "id": "tool_1", "input": map[string]any{}}),
		collectorDelta(0, map[string]any{"type": "text_delta", "text": "🙂"}),
		collectorDelta(1, map[string]any{"type": "thinking_delta", "thinking": "考"}),
		collectorDelta(2, map[string]any{"type": "input_json_delta", "partial_json": `{"path":"café",`}),
		collectorDelta(0, map[string]any{"type": "text_delta", "text": "x"}),
		collectorDelta(1, map[string]any{"type": "signature_delta", "signature": "ccgp1:σ"}),
		collectorDelta(2, map[string]any{"type": "input_json_delta", "partial_json": `"n":2}`}),
		{"content_block_stop", map[string]any{"index": 2}},
		collectorDelta(1, map[string]any{"type": "thinking_delta", "thinking": "🙂"}),
		collectorStart(3, map[string]any{"type": "text", "text": "unfinished:"}),
		collectorStart(4, map[string]any{"type": "thinking", "thinking": "継続:"}),
		collectorDelta(4, map[string]any{"type": "thinking_delta", "thinking": "中"}),
		collectorDelta(3, map[string]any{"type": "text_delta", "text": "partial"}),
	}
	for range repeatedDeltaCount {
		events = append(events,
			collectorDelta(0, map[string]any{"type": "text_delta", "text": "界"}),
			collectorDelta(1, map[string]any{"type": "thinking_delta", "thinking": "知"}),
		)
	}
	events = append(events,
		collectorTestEvent{"content_block_stop", map[string]any{"index": 0}},
		collectorTestEvent{"content_block_stop", map[string]any{"index": 1}},
	)
	c := &Collector{}
	for i, event := range events {
		c.Emit(event.name, event.data)
		if i == 1 {
			textStart["text"] = "changed source map"
		}
		if i == 2 {
			thinkingStart["thinking"] = "changed source map"
		}
	}
	if got, want := c.blocks[0]["text"], "Té🙂x"+strings.Repeat("界", repeatedDeltaCount); got != want {
		t.Fatalf("stopped text block = %#v, want %q", got, want)
	}
	if got, want := c.blocks[1]["thinking"], "思考🙂"+strings.Repeat("知", repeatedDeltaCount); got != want {
		t.Fatalf("stopped thinking block = %#v, want %q", got, want)
	}

	assertJSON := func(want string) {
		t.Helper()
		out, err := c.Result()
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("Result JSON = %s\nwant     = %s", got, want)
		}
	}
	want := fmt.Sprintf(`{"content":[{"text":"Té🙂x%s","type":"text"},{"signature":"ccgp1:σ","thinking":"思考🙂%s","type":"thinking"},{"id":"tool_1","input":{"n":2,"path":"café"},"type":"tool_use"},{"text":"unfinished:partial","type":"text"},{"thinking":"継続:中","type":"thinking"}],"id":"msg_1","model":"m","role":"assistant","stop_reason":null,"type":"message","usage":{"input_tokens":1,"output_tokens":2}}`, strings.Repeat("界", repeatedDeltaCount), strings.Repeat("知", repeatedDeltaCount))
	assertJSON(want)
	assertJSON(want)

	c.Emit("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": "!"}})
	c.Emit("content_block_delta", map[string]any{"index": 4, "delta": map[string]any{"type": "thinking_delta", "thinking": "終"}})
	assertJSON(fmt.Sprintf(`{"content":[{"text":"Té🙂x%s!","type":"text"},{"signature":"ccgp1:σ","thinking":"思考🙂%s","type":"thinking"},{"id":"tool_1","input":{"n":2,"path":"café"},"type":"tool_use"},{"text":"unfinished:partial","type":"text"},{"thinking":"継続:中終","type":"thinking"}],"id":"msg_1","model":"m","role":"assistant","stop_reason":null,"type":"message","usage":{"input_tokens":1,"output_tokens":2}}`, strings.Repeat("界", repeatedDeltaCount), strings.Repeat("知", repeatedDeltaCount)))
}

func TestCollectorTextThinkingAllocations(t *testing.T) {
	const size, deltaSize = 128 << 10, 64
	deltaText := strings.Repeat("x", deltaSize)
	for _, kind := range []string{"text", "thinking"} {
		t.Run(kind, func(t *testing.T) {
			field, deltaType := "text", "text_delta"
			if kind == "thinking" {
				field, deltaType = "thinking", "thinking_delta"
			}
			block := map[string]any{"type": kind, field: ""}
			delta := map[string]any{"type": deltaType, field: deltaText}
			events := make([]collectorTestEvent, 2+size/deltaSize)
			events[0] = collectorTestEvent{"message_start", map[string]any{"message": map[string]any{"model": "m"}}}
			events[1] = collectorStart(0, block)
			for i := 2; i < len(events); i++ {
				events[i] = collectorDelta(0, delta)
			}
			allocs := testing.AllocsPerRun(5, func() {
				c := &Collector{}
				emitCollectorEvents(c, events)
				if _, err := c.Result(); err != nil {
					panic(err)
				}
			})
			if allocs >= 128 {
				t.Fatalf("allocations per %d-byte result = %.1f, want < 128", size, allocs)
			}
		})
	}
}

func BenchmarkCollectorTextThinking(b *testing.B) {
	for _, size := range []int{16 << 10, 64 << 10, 256 << 10} {
		for _, kind := range []string{"text", "thinking"} {
			b.Run(fmt.Sprintf("%dKiB/%s", size>>10, kind), func(b *testing.B) {
				const deltaSize = 64
				field, deltaType := "text", "text_delta"
				if kind == "thinking" {
					field, deltaType = "thinking", "thinking_delta"
				}
				delta := map[string]any{"type": deltaType, field: strings.Repeat("x", deltaSize)}
				events := make([]collectorTestEvent, 2+size/deltaSize)
				events[0] = collectorTestEvent{"message_start", map[string]any{"message": map[string]any{"model": "m"}}}
				events[1] = collectorStart(0, map[string]any{"type": kind, field: ""})
				for i := 2; i < len(events); i++ {
					events[i] = collectorDelta(0, delta)
				}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					c := &Collector{}
					emitCollectorEvents(c, events)
					if _, err := c.Result(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
