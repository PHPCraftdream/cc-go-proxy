package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/clientver"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

const raiseConfigText = "codex.client_version:: 0.169.0\nproviders: [\n    {\n        name: codex\n        type: codex\n    }\n]\nprofiles: [\n    {\n        name: codex\n        provider: codex\n    }\n]\n"

func raiseFixture(t *testing.T, latest func(context.Context) (string, error)) (*config.Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(path, []byte(raiseConfigText), 0o600); err != nil {
		t.Fatal(err)
	}
	old := upgrader
	upgrader = &clientver.Upgrader{Latest: latest}
	t.Cleanup(func() { upgrader = old })
	cfg := &config.Config{
		ConfigFile: path,
		Profile:    "codex", ProviderName: "codex", CodexClientVersionSet: "0.169.0",
		Providers: map[string]*config.Provider{"codex": {Name: "codex", Type: "codex"}},
		Profiles:  map[string]*config.Profile{"codex": {Name: "codex", Provider: "codex"}},
	}
	return cfg, path
}

func TestRaiseClientVersionNotes(t *testing.T) {
	t.Run("env-only mode has nothing to write", func(t *testing.T) {
		note, raised := raiseClientVersion(&config.Config{}, nil, "")
		if raised || !strings.Contains(note, "env-only mode") {
			t.Errorf("%v %q", raised, note)
		}
	})
	t.Run("lookup failure points at the config", func(t *testing.T) {
		cfg, path := raiseFixture(t, func(context.Context) (string, error) { return "", errors.New("offline") })
		note, raised := raiseClientVersion(cfg, nil, "")
		if raised || !strings.Contains(note, "offline") || !strings.Contains(note, path) || !strings.Contains(note, "ccgp --models apply") {
			t.Errorf("%v %q", raised, note)
		}
		if got, _ := os.ReadFile(path); string(got) != raiseConfigText {
			t.Errorf("the config changed:\n%s", got)
		}
	})
	t.Run("nothing newer is said plainly", func(t *testing.T) {
		cfg, path := raiseFixture(t, nil)
		note, raised := raiseClientVersion(cfg, nil, "0.100.0")
		if raised || !strings.Contains(note, "the newest Codex CLI release") {
			t.Errorf("%v %q", raised, note)
		}
		if got, _ := os.ReadFile(path); string(got) != raiseConfigText {
			t.Errorf("the config changed:\n%s", got)
		}
	})
	t.Run("a raise survives a mapping refresh that cannot run", func(t *testing.T) {
		cfg, path := raiseFixture(t, nil)
		noLogin := &oauth.Session{Store: oauth.Store{Path: filepath.Join(t.TempDir(), "none.json")}, Client: &oauth.Client{}}
		note, raised := raiseClientVersion(cfg, noLogin, "0.171.0")
		if !raised || !strings.Contains(note, "0.169.0 -> 0.171.0") || !strings.Contains(note, "model mappings not refreshed") {
			t.Errorf("%v %q", raised, note)
		}
		if got, _ := os.ReadFile(path); !strings.Contains(string(got), "codex.client_version:: 0.171.0") {
			t.Errorf("the config was not raised:\n%s", got)
		}
	})
}
