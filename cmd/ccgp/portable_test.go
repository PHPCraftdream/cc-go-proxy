package main

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/x509roots/fallback/bundle"
)

// A system without root certificates (busybox, scratch, other minimal
// containers) must still reach HTTPS: ccgp carries a fallback bundle, which
// crypto/x509 uses only when the system has none.
func TestFallbackRootsAreCarried(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `_ "golang.org/x/crypto/x509roots/fallback"`) {
		t.Fatal("main.go no longer imports the fallback root certificates")
	}
	n := 0
	for range bundle.Roots() {
		n++
	}
	if n < 100 {
		t.Errorf("the fallback bundle holds %d roots, want the full Mozilla set", n)
	}
}
