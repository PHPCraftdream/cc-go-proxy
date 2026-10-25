package daemon

import (
	"encoding/json"
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

func TestHealthNewFields(t *testing.T) {
	b := []byte(`{"status":"ok","service":"ccgp","version":"0.1.0","pid":1}`)
	var h Health
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	if h.Exe != "" || h.ExeMod != 0 || h.Active != 0 {
		t.Errorf("old payload should decode to zero new fields: %+v", h)
	}
	b, err := json.Marshal(Health{Status: "ok", Service: "ccgp", Version: "0.1.0", PID: 2, Exe: "/ccgp", ExeMod: 5, Active: 3})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"exe": "/ccgp", "exe_mod": float64(5), "active": float64(3)} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
}
