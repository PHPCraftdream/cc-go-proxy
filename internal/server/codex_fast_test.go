package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/coder/websocket"
)

func TestFastRequestPoliciesAndFallback(t *testing.T) {
	var mu sync.Mutex
	var catalogCalls, handshakes int
	var requests []string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			mu.Lock()
			catalogCalls++
			mu.Unlock()
			_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6.2-sol","service_tiers":[{"id":"priority-fixture","name":"Fast"}]},{"slug":"gpt-6.2-terra","additional_speed_tiers":["fast"]}]}`)
			return
		}
		// Refuse the websocket handshake to exercise the HTTP fallback.
		if r.Header.Get("Upgrade") != "" {
			mu.Lock()
			handshakes++
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\"}}\n\n")
	})
	h := newCodexWSTestHarness(t, backend, "auto")
	h.cfg.Profiles = map[string]*config.Profile{"codex": {SpeedRules: map[string]string{"sol": "fast"}}}
	body := func(model, speed string) string {
		return fmt.Sprintf(`{"model":%q,"speed":%q,"stream":true,"metadata":{"user_id":"fast-policy"},"messages":[{"role":"user","content":"hi"}]}`, model, speed)
	}
	for i := 0; i < 2; i++ {
		if status, reply := h.post(t, body("gpt-6.2-sol", "standard")); status != 200 {
			t.Fatalf("fast override: %d %s", status, reply)
		}
	}
	mu.Lock()
	if handshakes == 0 || catalogCalls != 1 || len(requests) != 2 || !strings.Contains(requests[0], `"service_tier":"priority-fixture"`) || !strings.Contains(requests[1], `"service_tier":"priority-fixture"`) {
		t.Fatalf("fast: catalog=%d requests=%v", catalogCalls, requests)
	}
	mu.Unlock()
	h.cfg.Profiles["codex"].SpeedRules["sol"] = "standard"
	if status, reply := h.post(t, body("gpt-6.2-sol", "fast")); status != 200 {
		t.Fatalf("standard override: %d %s", status, reply)
	}
	mu.Lock()
	if catalogCalls != 1 || len(requests) != 3 || strings.Contains(requests[2], "service_tier") {
		t.Fatalf("standard: catalog=%d requests=%v", catalogCalls, requests)
	}
	mu.Unlock()
	if status, _ := h.post(t, body("gpt-6.2-terra", "fast")); status != 400 {
		t.Fatalf("unsupported status %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 || catalogCalls != 2 {
		t.Fatalf("unsupported called inference: catalog=%d requests=%v", catalogCalls, requests)
	}
}

func TestPersistedFastPolicyRequests(t *testing.T) {
	var mu sync.Mutex
	var catalogs int
	var requests []string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+codexWSTestAccessToken {
			t.Errorf("missing synthetic OAuth authorization on %s", r.URL.Path)
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/models") {
			catalogs++
			_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6.2-astra","service_tiers":[{"id":"fixture-priority","name":"Fast"}]},{"slug":"gpt-6.3-astra","service_tiers":[{"id":"fixture-new","name":"Fast"}]}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\"}}\n\n")
	})
	h := newCodexWSTestHarness(t, backend, "http")
	path := filepath.Join(t.TempDir(), "config.ktav")
	text := fmt.Sprintf("providers: [\n    {\n        name: codex\n        type: codex\n        base_url: %s\n    }\n]\nprofiles: [\n    {\n        name: codex\n        provider: codex\n    }\n    {\n        name: other\n        provider: codex\n    }\n]\n", h.backend.URL)
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, path)
	edit := func(value string) string {
		t.Helper()
		backup, err := config.EditFile(path, func(s string) (string, error) {
			return config.SetProfileSpeedText(s, "codex", "astra", value)
		})
		if err != nil {
			t.Fatal(err)
		}
		return backup
	}
	post := func(profile, model, speed, tier string, wantCatalogs int) {
		t.Helper()
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		selected, err := cfg.ForProfile(profile, "")
		if err != nil {
			t.Fatal(err)
		}
		selected.CodexTransport = "http"
		*h.cfg = *selected
		body := fmt.Sprintf(`{"model":%q,"speed":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, model, speed)
		if status, reply := h.post(t, body); status != 200 {
			t.Fatalf("%s %s: %d %s", profile, speed, status, reply)
		}
		mu.Lock()
		defer mu.Unlock()
		request := requests[len(requests)-1]
		if catalogs != wantCatalogs || !strings.Contains(request, `"model":"`+model+`"`) || (tier == "" && strings.Contains(request, `"service_tier"`)) || (tier != "" && !strings.Contains(request, `"service_tier":"`+tier+`"`)) {
			t.Fatalf("catalogs=%d want=%d request=%s tier=%q", catalogs, wantCatalogs, request, tier)
		}
	}
	post("codex", "gpt-6.2-astra", "standard", "", 0)
	edit("fast")
	post("codex", "gpt-6.2-astra", "standard", "fixture-priority", 1)
	post("other", "gpt-6.2-astra", "standard", "", 1)
	post("codex", "gpt-6.3-astra", "standard", "fixture-new", 2)
	edit("standard")
	post("codex", "gpt-6.2-astra", "fast", "", 2)
	edit("")
	post("codex", "gpt-6.2-astra", "standard", "", 2)
	post("codex", "gpt-6.2-astra", "fast", "fixture-priority", 2)
	if backup := edit(""); backup != "" {
		t.Fatal("repeated unset created a backup")
	}
	post("other", "gpt-6.2-astra", "standard", "", 2)
}

func TestFastWebsocketSwitch(t *testing.T) {
	var mu sync.Mutex
	var frames []string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6.2-sol","service_tiers":[{"id":"priority-fixture","name":"Fast"}]}]}`)
			return
		}
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		for i := 0; i < 2; i++ {
			frame := readCodexWSTestFrame(t, conn)
			if frame == nil {
				return
			}
			mu.Lock()
			frames = append(frames, string(frame))
			mu.Unlock()
			writeCodexWSTestEvents(t, conn, `{"type":"response.created","response":{"id":"r"}}`, `{"type":"response.completed","response":{"id":"r"}}`)
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	for _, speed := range []string{"standard", "fast"} {
		body := fmt.Sprintf(`{"model":"gpt-6.2-sol","speed":%q,"stream":true,"metadata":{"user_id":"tier-switch"},"messages":[{"role":"user","content":"hello"}]}`, speed)
		if status, reply := h.post(t, body); status != 200 {
			t.Fatalf("%s: %d %s", speed, status, reply)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(frames) != 2 || strings.Contains(frames[0], "service_tier") || !strings.Contains(frames[1], `"service_tier":"priority-fixture"`) || strings.Contains(frames[1], "previous_response_id") {
		t.Fatalf("frames: %v", frames)
	}
}
