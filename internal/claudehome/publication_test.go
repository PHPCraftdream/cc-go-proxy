package claudehome

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type publicationReader struct {
	read func() (string, error)
}

func (r *publicationReader) Read(p []byte) (int, error) {
	if r.read == nil {
		return 0, io.EOF
	}
	read := r.read
	r.read = nil
	s, err := read()
	return copy(p, s), err
}

func TestPublicationDoesNotExposePartialContents(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "state")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := dst + ".tmp"
	if err := os.WriteFile(legacy, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &publicationReader{read: func() (string, error) {
		if got, err := os.ReadFile(dst); err != nil || string(got) != "old" {
			t.Fatalf("during staging: %q, %v", got, err)
		}
		if err := writeIfChanged(dst, []byte("other complete")); err != nil {
			t.Fatal(err)
		}
		return "new complete", nil
	}}
	if err := publishFile(dst, r, nil, false); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "new complete" {
		t.Fatalf("published: %q, %v", got, err)
	}
	if got, err := os.ReadFile(legacy); err != nil || string(got) != "untouched" {
		t.Fatalf("legacy temp: %q, %v", got, err)
	}
	assertNoStages(t, dir)
}

func TestPublicationFailureKeepsDestination(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "state")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("read failure")
	r := &publicationReader{read: func() (string, error) { return "partial", failure }}
	if err := publishFile(dst, r, nil, false); !errors.Is(err, failure) {
		t.Fatalf("got %v", err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "old" {
		t.Fatalf("destination: %q, %v", got, err)
	}
	assertNoStages(t, dir)
}

func TestSeedPublicationDoesNotClobberConcurrentCreation(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "state")
	r := &publicationReader{read: func() (string, error) {
		if err := os.WriteFile(dst, []byte("created by session"), 0o600); err != nil {
			t.Fatal(err)
		}
		return "seed", nil
	}}
	if err := publishFile(dst, r, nil, true); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "created by session" {
		t.Fatalf("destination: %q, %v", got, err)
	}
	assertNoStages(t, dir)
}

func TestSeedStateCreatesAndPreservesState(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "state")
	if err := os.WriteFile(src, []byte(`{"theme":"dark","oauthAccount":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedState(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !strings.Contains(string(got), `"theme": "dark"`) || strings.Contains(string(got), "secret") {
		t.Fatalf("seed: %q, %v", got, err)
	}
	if err := os.WriteFile(dst, []byte("session state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedState(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "session state" {
		t.Fatalf("existing state: %q, %v", got, err)
	}
	assertNoStages(t, dir)
}

func assertNoStages(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".ccgp-stage-") {
			t.Fatalf("staged file left behind: %s", entry.Name())
		}
	}
}
