package server

import (
	"bytes"
	"testing"
	"time"
)

func TestSpeedKindFromTransportNote(t *testing.T) {
	for note, want := range map[string]string{
		"":                                       speedHTTP,
		"fallback to HTTP (handshake HTTP 404)":  speedHTTP,
		"transport websocket (request canceled)": speedHTTP,
		"transport websocket (full: new connection, sent 4.7 MiB)": speedWSFull,
		"transport websocket (full: connection failed)":            speedWSFull,
		"transport websocket (delta: sent 3.0 KiB of 4.7 MiB)":     speedWSDelta,
	} {
		if got := speedKind(note); got != want {
			t.Errorf("speedKind(%q) = %q, want %q", note, got, want)
		}
	}
}

func TestSpeedStatsLine(t *testing.T) {
	s := &speedStats{}
	if got := s.Take(); got != "" {
		t.Fatalf("empty stats = %q", got)
	}
	s.Add(speedWSDelta, 900*time.Millisecond, 7*time.Second)
	s.Add(speedHTTP, 3*time.Second, 9*time.Second)
	s.Add(speedHTTP, 1*time.Second, 8*time.Second)
	s.Add(speedHTTP, 2*time.Second, 20*time.Second)
	s.Add(speedWSFull, 0, 5*time.Second) // no content: not a sample
	want := "first token / whole reply, median: http 2s / 9s (3 replies), websocket delta 900ms / 7s (1 reply)"
	if got := s.Take(); got != want {
		t.Fatalf("line = %q\nwant   %q", got, want)
	}
	if got := s.Take(); got != "" {
		t.Fatalf("Take did not start again: %q", got)
	}
}

func TestSpeedStatsBounded(t *testing.T) {
	s := &speedStats{}
	for i := 0; i < speedSamplesMax+10; i++ {
		s.Add(speedHTTP, time.Second, 2*time.Second)
	}
	if n := len(s.first[speedHTTP]); n != speedSamplesMax {
		t.Fatalf("kept %d samples, want %d", n, speedSamplesMax)
	}
}

func TestWriteSpeedStatsSilentWhenEmpty(t *testing.T) {
	old := replySpeed
	replySpeed = &speedStats{}
	t.Cleanup(func() { replySpeed = old })
	var buf bytes.Buffer
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	writeSpeedStats(&buf, now)
	if buf.Len() != 0 {
		t.Fatalf("wrote %q with nothing to say", buf.String())
	}
	replySpeed.Add(speedWSFull, 1500*time.Millisecond, 4*time.Second)
	writeSpeedStats(&buf, now)
	want := "[12:00:00] upstream speed [codex]: first token / whole reply, median: websocket full 1.5s / 4s (1 reply)\n"
	if buf.String() != want {
		t.Fatalf("line = %q, want %q", buf.String(), want)
	}
}
