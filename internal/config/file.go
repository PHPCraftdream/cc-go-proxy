package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	ktav "github.com/ktav-lang/golang"
)

// ConfigFileEnv overrides the Ktav config file location.
const ConfigFileEnv = "CCGP_CONFIG"

// Provider types.
const (
	ProviderTypeOpenAI = "openai" // OpenAI-compatible Chat Completions, API key
	ProviderTypeCodex  = "codex"  // ChatGPT subscription via OAuth, Responses API
)

// reservedProfiles collide with `ccgp <command>`.
var reservedProfiles = map[string]bool{
	"serve": true, "start": true, "stop": true, "status": true,
	"login": true, "help": true, "version": true,
}

// ModelRule maps Claude model names matching a glob to a backend model.
type ModelRule struct {
	Match string `json:"match"`
	Model string `json:"model"`
}

// Provider is an upstream backend.
type Provider struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	// APIKeyEnv names an env var holding the key; otherwise the creds store is used.
	APIKeyEnv string `json:"api_key_env"`
}

// Profile binds a URL token (managed in creds) to a provider and model mapping.
type Profile struct {
	Name         string      `json:"name"`
	Provider     string      `json:"provider"`
	DefaultModel string      `json:"default_model"`
	Models       []ModelRule `json:"models"`
}

// fileConfig is the Ktav config file shape.
type fileConfig struct {
	Listen struct {
		Host string `json:"host"`
		Port int64  `json:"port"`
	} `json:"listen"`
	Providers []Provider `json:"providers"`
	Profiles  []Profile  `json:"profiles"`
}

// DefaultConfigPath returns <UserConfigDir>/cc-go-proxy/config.ktav.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cc-go-proxy", "config.ktav"), nil
}

// configFilePath resolves the config path; explicit reports whether it was set via env.
func configFilePath() (p string, explicit bool, err error) {
	if p := os.Getenv(ConfigFileEnv); p != "" {
		return p, true, nil
	}
	p, err = DefaultConfigPath()
	return p, false, err
}

// loadFile parses and validates a Ktav config file. Missing file returns (nil, nil).
func loadFile(p string) (*fileConfig, error) {
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var fc fileConfig
	if err := ktav.LoadsInto(string(data), &fc); err != nil {
		return nil, fmt.Errorf("parse %s: %w (hint: write numeric-looking strings as `key:: value`)", p, err)
	}
	if err := fc.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &fc, nil
}

func (fc *fileConfig) validate() error {
	providers := map[string]bool{}
	for i, pr := range fc.Providers {
		if pr.Name == "" {
			return fmt.Errorf("providers[%d]: name is required", i)
		}
		if providers[pr.Name] {
			return fmt.Errorf("providers[%d]: duplicate name %q", i, pr.Name)
		}
		if pr.Type != ProviderTypeOpenAI && pr.Type != ProviderTypeCodex {
			return fmt.Errorf("provider %q: type must be %q or %q", pr.Name, ProviderTypeOpenAI, ProviderTypeCodex)
		}
		providers[pr.Name] = true
	}
	profiles := map[string]bool{}
	for i, pf := range fc.Profiles {
		if pf.Name == "" {
			return fmt.Errorf("profiles[%d]: name is required", i)
		}
		if profiles[pf.Name] {
			return fmt.Errorf("profiles[%d]: duplicate name %q", i, pf.Name)
		}
		if reservedProfiles[pf.Name] {
			return fmt.Errorf("profiles[%d]: %q is a ccgp command name", i, pf.Name)
		}
		if !providers[pf.Provider] {
			return fmt.Errorf("profile %q: unknown provider %q", pf.Name, pf.Provider)
		}
		for j, r := range pf.Models {
			if r.Match == "" || r.Model == "" {
				return fmt.Errorf("profile %q: models[%d]: match and model are required", pf.Name, j)
			}
			if _, err := path.Match(strings.ToLower(r.Match), ""); err != nil {
				return fmt.Errorf("profile %q: models[%d]: bad pattern %q: %w", pf.Name, j, r.Match, err)
			}
		}
		profiles[pf.Name] = true
	}
	return nil
}

// applyFile fills cfg from fc; HOST/PORT from env win.
func applyFile(cfg *Config, fc *fileConfig) {
	if os.Getenv("HOST") == "" && fc.Listen.Host != "" {
		cfg.Host = fc.Listen.Host
	}
	if os.Getenv("PORT") == "" && fc.Listen.Port != 0 {
		cfg.Port = strconv.FormatInt(fc.Listen.Port, 10)
	}
	cfg.Providers = map[string]*Provider{}
	for i := range fc.Providers {
		p := fc.Providers[i]
		if p.Type == ProviderTypeOpenAI && p.BaseURL == "" {
			p.BaseURL = "https://api.openai.com/v1"
		}
		cfg.Providers[p.Name] = &p
	}
	cfg.Profiles = map[string]*Profile{}
	for i := range fc.Profiles {
		p := fc.Profiles[i]
		cfg.Profiles[p.Name] = &p
	}
}

// ForProfile returns a per-request copy routed to the profile's provider.
// apiKey is used for openai providers.
func (c *Config) ForProfile(profile string, apiKey string) (*Config, error) {
	pf, ok := c.Profiles[profile]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", profile)
	}
	pr := c.Providers[pf.Provider]
	out := *c
	out.Profile = pf.Name
	out.ProviderName = pr.Name
	out.ProviderType = pr.Type
	out.ModelRules = pf.Models
	out.DefaultModel = pf.DefaultModel
	// Tier env overrides apply only in env-only mode.
	out.OpusModel, out.SonnetModel, out.HaikuModel = "", "", ""
	if pr.Type == ProviderTypeOpenAI {
		out.OpenAIBaseURL = pr.BaseURL
		out.OpenAIAPIKey = apiKey
	}
	return &out, nil
}

// ResolveModel applies ordered rules, then default_model. ok=false: no match.
func (c *Config) ResolveModel(claudeModel string) (model string, ok bool) {
	name := strings.ToLower(claudeModel)
	for _, r := range c.ModelRules {
		if matched, _ := path.Match(strings.ToLower(r.Match), name); matched {
			return r.Model, true
		}
	}
	if c.DefaultModel != "" {
		return c.DefaultModel, true
	}
	return "", false
}
