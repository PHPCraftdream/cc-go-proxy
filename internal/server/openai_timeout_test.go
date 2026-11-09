package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
)

func TestDoOpenAIRequestDeadlineCoversBodyRead(t *testing.T) {
	requestSeen := make(chan struct{})
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(requestSeen)
		<-r.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()

	client := &http.Client{Transport: connpool.NewRouter(0, 1, 1, nil)}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := doOpenAIRequest(context.Background(), client, req, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("doOpenAIRequest() error before headers = %v", err)
	}
	defer resp.Body.Close()
	<-requestSeen

	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("body read error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("body read did not stop at the request deadline")
	}
	select {
	case <-requestCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request context was not canceled at the deadline")
	}
}

func TestDoOpenAIRequestBodyCloseCancelsContext(t *testing.T) {
	requestSeen := make(chan struct{})
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(requestSeen)
		<-r.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()

	client := &http.Client{Transport: connpool.NewRouter(0, 1, 1, nil)}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := doOpenAIRequest(context.Background(), client, req, 5*time.Second)
	if err != nil {
		t.Fatalf("doOpenAIRequest() error = %v", err)
	}
	defer resp.Body.Close()
	defer func() {
		if !errors.Is(resp.Request.Context().Err(), context.Canceled) {
			t.Errorf("request context error after body close = %v, want context.Canceled", resp.Request.Context().Err())
		}
	}()
	<-requestSeen
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("response body close error = %v", err)
	}
	select {
	case <-requestCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("closing response body did not cancel the upstream request")
	}
}
