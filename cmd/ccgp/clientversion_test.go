package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/clientver"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

const clientVersionProviders = "providers: [\n    {\n        name: codex\n        type: codex\n    }\n]\nprofiles: [\n    {\n        name: codex\n        provider: codex\n    }\n]\n"

func clientVersionConfig(t *testing.T, body string) (*config.Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, path)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

func stubLatestRelease(t *testing.T, latest func(context.Context) (string, error)) {
	t.Helper()
	old := clientver.Default
	clientver.Default = &clientver.Upgrader{Latest: latest}
	t.Cleanup(func() { clientver.Default = old })
}

func fixedRelease(v string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return v, nil }
}

func TestClientVersionApplyRaisesTheConfig(t *testing.T) {
	cfg, path := clientVersionConfig(t, "codex.client_version:: 0.169.0\n"+clientVersionProviders)
	stubLatestRelease(t, fixedRelease("0.170.0"))
	var out bytes.Buffer
	clientVersion(context.Background(), cfg, true, &out)
	if !strings.Contains(out.String(), "raised 0.169.0 -> 0.170.0") || cfg.CodexClientVersion() != "0.170.0" {
		t.Fatalf("output %q, version %s", out.String(), cfg.CodexClientVersion())
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "codex.client_version:: 0.170.0") {
		t.Errorf("config:\n%s", got)
	}
	if backups, _ := filepath.Glob(path + ".bak-*"); len(backups) != 1 {
		t.Errorf("backups: %v", backups)
	}
}

// The read-only form reads the catalog as the newer version and writes nothing.
func TestClientVersionShowDoesNotWrite(t *testing.T) {
	cfg, path := clientVersionConfig(t, "codex.client_version:: 0.169.0\n"+clientVersionProviders)
	before, _ := os.ReadFile(path)
	stubLatestRelease(t, fixedRelease("0.170.0"))
	var out bytes.Buffer
	clientVersion(context.Background(), cfg, false, &out)
	if !strings.Contains(out.String(), "Codex CLI 0.170.0 is out") || cfg.CodexClientVersion() != "0.170.0" {
		t.Fatalf("output %q, version %s", out.String(), cfg.CodexClientVersion())
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Errorf("the config changed:\n%s", after)
	}
	if backups, _ := filepath.Glob(path + ".bak-*"); len(backups) != 0 {
		t.Errorf("backups: %v", backups)
	}
}

func TestClientVersionNothingNewerIsQuiet(t *testing.T) {
	for _, latest := range []string{"0.169.0", "0.100.0"} {
		cfg, path := clientVersionConfig(t, "codex.client_version:: 0.169.0\n"+clientVersionProviders)
		before, _ := os.ReadFile(path)
		stubLatestRelease(t, fixedRelease(latest))
		var out bytes.Buffer
		clientVersion(context.Background(), cfg, true, &out)
		if out.Len() != 0 || cfg.CodexClientVersion() != "0.169.0" {
			t.Errorf("latest %s: output %q, version %s", latest, out.String(), cfg.CodexClientVersion())
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
			t.Errorf("latest %s: the config changed", latest)
		}
	}
}

func TestClientVersionLookupFailureKeepsTheConfigured(t *testing.T) {
	cfg, path := clientVersionConfig(t, "codex.client_version:: 0.169.0\n"+clientVersionProviders)
	before, _ := os.ReadFile(path)
	stubLatestRelease(t, func(context.Context) (string, error) { return "", errors.New("offline") })
	var out bytes.Buffer
	clientVersion(context.Background(), cfg, true, &out)
	if !strings.Contains(out.String(), "could not check for a newer Codex CLI (offline)") || cfg.CodexClientVersion() != "0.169.0" {
		t.Fatalf("output %q, version %s", out.String(), cfg.CodexClientVersion())
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("the config changed")
	}
}

// A config without the key gets the built-in default first, then the newer release.
func TestClientVersionSeedsThenRaises(t *testing.T) {
	cfg, path := clientVersionConfig(t, clientVersionProviders)
	stubLatestRelease(t, fixedRelease("0.171.0"))
	var out bytes.Buffer
	clientVersion(context.Background(), cfg, true, &out)
	text := out.String()
	if !strings.Contains(text, "codex.client_version "+config.DefaultCodexClientVersion+" written to") || !strings.Contains(text, "raised "+config.DefaultCodexClientVersion+" -> 0.171.0") {
		t.Fatalf("output %q", text)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "codex.client_version:: 0.171.0") {
		t.Errorf("config:\n%s", got)
	}
}

// Without a codex provider nothing is written, whatever the release says.
func TestClientVersionNeedsACodexProvider(t *testing.T) {
	cfg, path := clientVersionConfig(t, "providers: [\n    {\n        name: p\n        type: codex_api\n    }\n]\n")
	before, _ := os.ReadFile(path)
	stubLatestRelease(t, fixedRelease(config.DefaultCodexClientVersion))
	var out bytes.Buffer
	clientVersion(context.Background(), cfg, true, &out)
	if out.Len() != 0 {
		t.Errorf("output %q", out.String())
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Errorf("the config changed:\n%s", after)
	}
}
