package server

import (
	"bufio"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

func TestHealthPayloadJSONFields(t *testing.T) {
	f := &inFlight{}
	b, err := json.Marshal(healthPayload(f, "/some/ccgp", 12345))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"status", "service", "version", "pid", "exe", "exe_mod", "active"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing field %q in %s", k, b)
		}
	}
	if got["exe"] != "/some/ccgp" || got["exe_mod"] != float64(12345) || got["active"] != float64(0) {
		t.Errorf("unexpected values: %s", b)
	}
}

func TestExeInfoReportsTestBinary(t *testing.T) {
	path, mod := exeInfo()
	if path == "" {
		t.Fatal("exe path is empty")
	}
	if mod == 0 {
		t.Error("mod time of the test binary should be known")
	}
}

func TestInFlightMiddlewareCountsCallRequests(t *testing.T) {
	f := &inFlight{}
	app := fiber.New()
	app.Use(f.middleware())
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(healthPayload(f, "", 0))
	})
	release := make(chan struct{})
	app.Post(callPrefix+":token/v1/messages", func(c *fiber.Ctx) error {
		<-release
		return c.SendString("ok")
	})

	done := make(chan error, 1)
	go func() {
		_, err := app.Test(httptest.NewRequest("POST", "/call/tok/v1/messages", nil), -1)
		done <- err
	}()

	waitForCount(t, f, 1)

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h["active"] != float64(1) {
		t.Errorf("active = %v, want 1 while request in flight", h["active"])
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := f.count(); n != 0 {
		t.Errorf("active = %d after completion, want 0", n)
	}
}

// TestInFlightSurvivesHandlerReturnThroughStreamWriter is the streaming case:
// the handler returns immediately after installing a body stream writer, and
// the request must stay counted until the writer finishes.
func TestInFlightSurvivesHandlerReturnThroughStreamWriter(t *testing.T) {
	f := &inFlight{}
	app := fiber.New()
	app.Use(f.middleware())
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(healthPayload(f, "", 0))
	})
	writerStarted := make(chan struct{})
	release := make(chan struct{})
	app.Post(callPrefix+":token/v1/messages", func(c *fiber.Ctx) error {
		rel := deferActive(c)
		c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
			defer rel()
			close(writerStarted)
			<-release
			_, _ = w.WriteString("done")
		})
		return nil
	})

	done := make(chan error, 1)
	go func() {
		_, err := app.Test(httptest.NewRequest("POST", "/call/tok/v1/messages", nil), -1)
		done <- err
	}()

	// The handler has returned (counter would have dropped without the
	// takeover); the writer is running and must keep the request counted.
	select {
	case <-writerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("stream writer never started")
	}
	waitForCount(t, f, 1)

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h["active"] != float64(1) {
		t.Errorf("active = %v, want 1 while stream writer runs", h["active"])
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream request never finished")
	}
	waitForCount(t, f, 0)
}

// TestInFlightReleasesOnHandlerReturnAndNeverBelowZero: a plain handler
// releases on return, and repeated releases cannot push the counter negative.
func TestInFlightReleasesOnHandlerReturnAndNeverBelowZero(t *testing.T) {
	f := &inFlight{}
	app := fiber.New()
	app.Use(f.middleware())
	app.Post(callPrefix+":token/v1/messages", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})
	if _, err := app.Test(httptest.NewRequest("POST", "/call/tok/v1/messages", nil)); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, f, 0)

	// A taken-over release can be called twice and the middleware releases
	// again after the handler returns: the guard must keep the count at 0.
	app.Post(callPrefix+":token/twice/v1/messages", func(c *fiber.Ctx) error {
		rel := deferActive(c)
		rel()
		rel()
		return c.SendString("ok")
	})
	if _, err := app.Test(httptest.NewRequest("POST", "/call/tok/twice/v1/messages", nil)); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, f, 0)
}

func TestInFlightMiddlewareIgnoresHealth(t *testing.T) {
	f := &inFlight{}
	app := fiber.New()
	app.Use(f.middleware())
	started := make(chan struct{})
	block := make(chan struct{})
	app.Get("/health", func(c *fiber.Ctx) error {
		if n := f.count(); n != 0 {
			t.Errorf("/health counted as in flight while open: %d", n)
		}
		close(started)
		<-block
		return c.SendString("ok")
	})

	done := make(chan error, 1)
	go func() {
		_, err := app.Test(httptest.NewRequest("GET", "/health", nil), -1)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("health handler never started")
	}
	waitForCount(t, f, 0)

	close(block)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("health request never finished")
	}
}

// waitForCount polls until the counter reaches want; a failing request would
// never reach it, so the wait must be bounded.
func waitForCount(t *testing.T, f *inFlight, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.count() != want {
		if time.Now().After(deadline) {
			t.Fatalf("in-flight count = %d, want %d", f.count(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
