package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

func TestStreamOpenAIResponseStopsAfterContentWriterFailure(t *testing.T) {
	const firstText = "FIRST_CONTENT_DELTA_SENTINEL"
	writeErr := errors.New("client write failed")

	for _, tc := range []struct {
		name       string
		bufferSize int
		firstText  string
	}{
		{
			name:       "small buffered flush",
			bufferSize: 4096,
			firstText:  firstText,
		},
		{
			name:       "large content direct write",
			bufferSize: 1,
			firstText:  firstText + strings.Repeat("x", 8192),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &disconnectChunkReader{chunks: [][]byte{
				[]byte(fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", tc.firstText)),
				[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"second delta\"}}]}\n\n"),
				[]byte("data: [DONE]\n\n"),
			}}
			body := &disconnectTrackingBody{Reader: reader}
			resp := &http.Response{Body: body}
			ctx, cancel := context.WithCancel(context.Background())
			writer := &sentinelArmedWriter{sentinel: firstText, failErr: writeErr}

			err := streamOpenAIResponse(bufio.NewWriterSize(writer, tc.bufferSize), resp, "test-model", &config.Config{}, time.Now(), cancel)
			if !errors.Is(err, writeErr) {
				t.Fatalf("streamOpenAIResponse() error = %v, want %v", err, writeErr)
			}
			if !body.closed {
				t.Error("response body was not closed after writer failure")
			}
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Errorf("context error = %v, want context.Canceled", ctx.Err())
			}
			if reader.reads != 2 {
				t.Errorf("upstream Read calls = %d, want exactly 2 (no read after writer failure)", reader.reads)
			}
			if got := writer.output.String(); !strings.Contains(got, firstText) {
				t.Errorf("successfully delivered output does not contain first content sentinel %q", firstText)
			}
			if writer.failedWrites != 1 {
				t.Errorf("failed writer calls = %d, want 1", writer.failedWrites)
			}
			if tc.name == "large content direct write" && len(tc.firstText) <= tc.bufferSize {
				t.Fatalf("large content length = %d, want it to exceed bufio buffer size %d", len(tc.firstText), tc.bufferSize)
			}
		})
	}
}

type disconnectChunkReader struct {
	chunks [][]byte
	reads  int
}

func (r *disconnectChunkReader) Read(p []byte) (int, error) {
	r.reads++
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	if len(p) < len(chunk) {
		return 0, fmt.Errorf("test reader buffer (%d) is smaller than SSE chunk (%d)", len(p), len(chunk))
	}
	r.chunks = r.chunks[1:]
	return copy(p, chunk), nil
}

type disconnectTrackingBody struct {
	io.Reader
	closed bool
}

func (b *disconnectTrackingBody) Close() error {
	b.closed = true
	return nil
}

type sentinelArmedWriter struct {
	sentinel     string
	failErr      error
	output       bytes.Buffer
	armed        bool
	failedWrites int
}

func (w *sentinelArmedWriter) Write(p []byte) (int, error) {
	if w.armed {
		w.failedWrites++
		return 0, w.failErr
	}
	n, err := w.output.Write(p)
	if strings.Contains(w.output.String(), w.sentinel) {
		w.armed = true
	}
	return n, err
}

func TestStreamOpenAIToClaudeReadErrorAfterFinishReasonIsError(t *testing.T) {
	const readFailure = "upstream read failed after finish_reason"
	reader := &finishReasonThenErrorReader{first: []byte("data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n")}
	var output bytes.Buffer
	err := streamOpenAIToClaude(bufio.NewWriter(&output), reader, "test-model", &config.Config{}, time.Now())
	if err != nil {
		t.Fatalf("streamOpenAIToClaude() error = %v, want read failure emitted as SSE", err)
	}
	events := parseStreamEvents(output.String())
	if len(events) == 0 || events[len(events)-1].name != "error" {
		t.Fatalf("final event = %#v, want error", events)
	}
	if !strings.Contains(output.String(), readFailure) {
		t.Errorf("error event %q does not include read failure %q", output.String(), readFailure)
	}
	for _, event := range events {
		if event.name == "message_delta" || event.name == "message_stop" {
			t.Fatalf("success event %q emitted after upstream read failure", event.name)
		}
	}
}

type finishReasonThenErrorReader struct {
	first []byte
	done  bool
}

func (r *finishReasonThenErrorReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.first), nil
	}
	return 0, errors.New("upstream read failed after finish_reason")
}

func TestStreamOpenAIToClaudeFinishReasonUsageTrailer(t *testing.T) {
	input := "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":4}}\n\n"
	var output bytes.Buffer
	if err := streamOpenAIToClaude(bufio.NewWriter(&output), strings.NewReader(input), "test-model", &config.Config{}, time.Now()); err != nil {
		t.Fatalf("streamOpenAIToClaude() error = %v", err)
	}

	for _, event := range parseStreamEvents(output.String()) {
		if event.name != "message_delta" {
			continue
		}
		usage, ok := event.data["usage"].(map[string]interface{})
		if !ok {
			t.Fatalf("message_delta usage = %#v, want usage object", event.data["usage"])
		}
		if usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(4) {
			t.Errorf("message_delta usage = %#v, want input_tokens=7 and output_tokens=4", usage)
		}
		return
	}
	t.Fatalf("no message_delta event in output: %q", output.String())
}
