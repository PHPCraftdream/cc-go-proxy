package daemon

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","service":"ccgp","pid":42}`))
	}))
	defer ok.Close()
	h, err := Check(ok.URL)
	if err != nil || h.PID != 42 {
		t.Fatalf("Check = %+v, %v", h, err)
	}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer other.Close()
	if _, err := Check(other.URL); err == nil {
		t.Error("foreign server accepted as ccgp")
	}
}

func TestStopNotRunning(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if err := Stop(url, time.Second); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Stop = %v, want ErrNotRunning", err)
	}
}

func TestOpenLogRotates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "logs", "ccgp.log")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, maxLogSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openLog(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if st, err := os.Stat(p + ".1"); err != nil || st.Size() != maxLogSize+1 {
		t.Errorf("rotated file: %v", err)
	}
	if st, _ := os.Stat(p); st.Size() != 0 {
		t.Errorf("new log size = %d", st.Size())
	}
}
