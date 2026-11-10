package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gofiber/fiber/v2"
)

func codexWSSideBody(userID string, messages string) string {
	return `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"` + userID + `"},"messages":[` + messages + `]}`
}

func codexWSSideUser(text string) string      { return `{"role":"user","content":"` + text + `"}` }
func codexWSSideAssistant(text string) string { return `{"role":"assistant","content":"` + text + `"}` }

// A background request of the client (a short fixed message after the same
// history) answered on the websocket replaces its chain; once ccgp has seen
// that, requests ending in the message go over HTTP and the conversation's
// chain survives them.
func TestCodexWebsocketBackgroundRequestsGoOverHTTPOnceLearned(t *testing.T) {
	var frames, posts atomic.Int32
	answers := []string{"a1", "s1", "a2"}
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			posts.Add(1)
			okStream(w)
			return
		}
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		for _, answer := range answers {
			readCodexWSTestFrame(t, conn)
			id := "side-response-" + answer
			frames.Add(1)
			writeCodexWSTestEvents(t, conn,
				`{"type":"response.created","response":{"id":"`+id+`"}}`,
				`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`+answer+`"}]}}`,
				`{"type":"response.completed","response":{"id":"`+id+`"}}`)
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	post := func(messages string) {
		t.Helper()
		if status, body := h.post(t, codexWSSideBody("side-session", messages)); status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
			t.Fatalf("status %d, body %q", status, body)
		}
	}
	main, a1, side := codexWSSideUser("main question"), codexWSSideAssistant("a1"), codexWSSideUser("background template")

	post(main)
	post(strings.Join([]string{main, a1, side}, ","))
	post(strings.Join([]string{main, a1, codexWSSideUser("real next")}, ","))
	if frames.Load() != 3 || posts.Load() != 0 {
		t.Fatalf("before learning: %d websocket frames and %d HTTP posts, want 3 and 0", frames.Load(), posts.Load())
	}
	if !strings.Contains(h.stream.String(), "requests ending in it go over HTTP from now on") {
		t.Errorf("the learned message was not announced: %q", h.stream.String())
	}

	post(strings.Join([]string{main, a1, codexWSSideUser("real next"), codexWSSideAssistant("a2"), side}, ","))
	if frames.Load() != 3 || posts.Load() != 1 {
		t.Fatalf("background request: %d websocket frames and %d HTTP posts, want 3 and 1", frames.Load(), posts.Load())
	}
	if !strings.Contains(h.access.String(), "transport http (side request)") {
		t.Errorf("access log lacks the side request note: %q", h.access.String())
	}
	var stats strings.Builder
	writeCodexWebsocketStats(&stats, time.Now())
	if !strings.Contains(stats.String(), "1 background request sent over HTTP") {
		t.Errorf("periodic line = %q", stats.String())
	}
}

// The subagents of a conversation share its cache key, system prompt and tools;
// each keeps its own websocket, so one does not replace the other's chain.
func TestCodexWebsocketAgentsOfOneConversationHaveTheirOwnConnection(t *testing.T) {
	var mu sync.Mutex
	var conns atomic.Int32
	var previous []string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		id := conns.Add(1)
		for exchange := 0; exchange < 2; exchange++ {
			frame := readCodexWSTestFrame(t, conn)
			var payload struct {
				Previous string `json:"previous_response_id"`
			}
			_ = json.Unmarshal(frame, &payload)
			mu.Lock()
			previous = append(previous, payload.Previous)
			mu.Unlock()
			responseID := fmt.Sprintf("agent-%d-%d", id, exchange)
			writeCodexWSTestEvents(t, conn,
				`{"type":"response.created","response":{"id":"`+responseID+`"}}`,
				`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"r"}]}}`,
				`{"type":"response.completed","response":{"id":"`+responseID+`"}}`)
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	post := func(messages ...string) {
		t.Helper()
		if status, body := h.post(t, codexWSSideBody("agents-session", strings.Join(messages, ","))); status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
			t.Fatalf("status %d, body %q", status, body)
		}
	}
	taskA, taskB, reply := codexWSSideUser("task of agent A"), codexWSSideUser("task of agent B"), codexWSSideAssistant("r")
	post(taskA)
	post(taskB)
	post(taskA, reply, codexWSSideUser("more for A"))
	post(taskB, reply, codexWSSideUser("more for B"))

	mu.Lock()
	got := append([]string(nil), previous...)
	mu.Unlock()
	if want := []string{"", "", "agent-1-0", "agent-2-0"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("previous_response_id of the frames = %q, want %q (full, full, then a delta on each agent's own connection)", got, want)
	}
	if conns.Load() != 2 {
		t.Errorf("handshakes = %d, want one per agent", conns.Load())
	}
	tags := regexp.MustCompile(`conv ([0-9a-f]{6})\.([0-9a-f]{4})`).FindAllStringSubmatch(h.access.String(), -1)
	if len(tags) != 4 || tags[0][1] != tags[1][1] || tags[0][2] == tags[1][2] || tags[0][2] != tags[2][2] || tags[1][2] != tags[3][2] {
		t.Errorf("conversation tags in the access log = %q, want one thread and two agent tags", tags)
	}
}
