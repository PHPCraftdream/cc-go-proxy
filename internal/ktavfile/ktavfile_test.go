package ktavfile

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type sample struct {
	ID      string    `json:"id,omitempty"`
	Secret  string    `json:"secret"`
	Account string    `json:"account,omitempty"`
	At      time.Time `json:"at"`
	N       int64     `json:"n"`
	Tags    []string  `json:"tags"`
}

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.ktav")
	in := sample{
		Secret: "1234567890", At: time.Now(), N: 1_700_000_000_123_456_789,
		Tags: []string{"a", "1", "true", ""},
	}
	if err := Write(path, in); err != nil {
		t.Fatal(err)
	}
	var out sample
	if err := Read(path, &out, nil); err != nil {
		t.Fatal(err)
	}
	if out.Secret != in.Secret || out.N != in.N || !out.At.Equal(in.At) || strings.Join(out.Tags, "|") != "a|1|true|" {
		t.Errorf("read back %+v from %+v", out, in)
	}
}

func TestWriteIsAtomicAndKtav(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.ktav")
	if err := Write(path, sample{Secret: "x", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(strings.TrimSpace(string(b)), "{") {
		t.Errorf("file looks like JSON:\n%s", b)
	}
}

func TestReadMissingIsNotExist(t *testing.T) {
	err := Read(filepath.Join(t.TempDir(), "none.ktav"), &sample{}, json.Unmarshal)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestReadCorruptIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.ktav")
	if err := os.WriteFile(path, []byte("a: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Read(path, &sample{}, json.Unmarshal)
	if err == nil || errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("err = %v", err)
	}
}

func TestReadMigratesLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth-x.ktav")
	old := LegacyPath(path)
	if filepath.Base(old) != "oauth-x.json" {
		t.Fatalf("legacy path = %s", old)
	}
	if err := os.WriteFile(old, []byte(`{"secret":"rt-1","n":7,"at":"2026-10-20T19:07:00Z","tags":["x"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var v sample
	if err := Read(path, &v, json.Unmarshal); err != nil {
		t.Fatal(err)
	}
	if v.Secret != "rt-1" || v.N != 7 {
		t.Errorf("migrated = %+v", v)
	}
	if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("legacy file kept: %v", err)
	}
	var again sample
	if err := Read(path, &again, nil); err != nil || again.Secret != "rt-1" || again.N != 7 || !again.At.Equal(v.At) {
		t.Errorf("after migration = %+v, %v", again, err)
	}
}

func TestReadKeepsLegacyWhenItIsBroken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.ktav")
	old := LegacyPath(path)
	if err := os.WriteFile(old, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Read(path, &sample{}, json.Unmarshal); err == nil {
		t.Fatal("broken legacy file migrated")
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("broken legacy file was removed: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a Ktav file was written from broken input: %v", err)
	}
}

func TestReadPrefersKtavOverLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.ktav")
	if err := Write(path, sample{Secret: "new", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LegacyPath(path), []byte(`{"secret":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var v sample
	if err := Read(path, &v, json.Unmarshal); err != nil || v.Secret != "new" {
		t.Errorf("read %+v, %v; want the Ktav file", v, err)
	}
}

func TestSameValueTreatsEmptyAndNilAlike(t *testing.T) {
	if !sameValue(sample{Tags: nil}, sample{Tags: []string{}}) {
		t.Error("nil and empty list differ")
	}
	if sameValue(sample{Secret: "a"}, sample{Secret: "b"}) {
		t.Error("different secrets compare equal")
	}
}

func TestWriteRetiresLegacyJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.ktav")
	if err := os.WriteFile(LegacyPath(path), []byte(`{"secret":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, sample{Secret: "new", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LegacyPath(path)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("legacy file kept after a write: %v", err)
	}
}
