package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustLoad loads text through the real loader or fails the test. The text is
// written as-is (CRLF stays CRLF): the loader accepts CRLF files.
func mustLoad(t *testing.T, text string) *fileConfig {
	t.Helper()
	fc, err := loadFile(writeKtav(t, text))
	if err != nil {
		t.Fatalf("result does not load: %v\n%s", err, text)
	}
	return fc
}

func profilePtr(fc *fileConfig, name string) *Profile {
	for i := range fc.Profiles {
		if fc.Profiles[i].Name == name {
			return &fc.Profiles[i]
		}
	}
	return nil
}

func providerPtr(fc *fileConfig, name string) *Provider {
	for i := range fc.Providers {
		if fc.Providers[i].Name == name {
			return &fc.Providers[i]
		}
	}
	return nil
}

func TestValidateProfileName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr string // "" means valid
	}{
		{"codex", ""},
		{"a", ""},
		{"a-b_c2", ""},
		{strings.Repeat("a", 32), ""}, // 32 chars: still valid
		{"", "empty"},
		{"Codex", "[a-z0-9_-]"},
		{"-x", "[a-z0-9_-]"},
		{"a b", "[a-z0-9_-]"},
		{strings.Repeat("a", 33), "[a-z0-9_-]"}, // 33 chars: too long
		{"serve", "command"},
		{"profile", "command"},
		{"send", "command"},
	}
	for _, tt := range tests {
		err := ValidateProfileName(tt.name)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("ValidateProfileName(%q) = %v, want nil", tt.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("ValidateProfileName(%q) = %v, want it to mention %q", tt.name, err, tt.wantErr)
		}
	}
}

func TestNextFreeName(t *testing.T) {
	tests := []struct {
		base  string
		taken []string
		want  string
	}{
		{"codex", nil, "codex"},
		{"codex", []string{"codex"}, "codex2"},
		{"codex", []string{"codex", "codex2", "codex3"}, "codex4"},
		{"codex2", nil, "codex2"},
		{"codex2", []string{"codex"}, "codex2"},
		{"codex2", []string{"codex", "codex2"}, "codex3"},
		{"2024", []string{"2024"}, "20242"},
	}
	for _, tt := range tests {
		taken := map[string]bool{}
		for _, s := range tt.taken {
			taken[s] = true
		}
		if got := NextFreeName(tt.base, func(n string) bool { return taken[n] }); got != tt.want {
			t.Errorf("NextFreeName(%q, %v) = %q, want %q", tt.base, tt.taken, got, tt.want)
		}
	}
}

func TestAddProfileToTemplate(t *testing.T) {
	base := string(DefaultTemplate())
	pf := ProfileSpec{
		Name:     "work",
		Provider: "zai",
		Rules: []ModelRule{
			{Match: "*", Model: "glm-5.3-flash", ContextWindow: 1000000},
			{Match: "*x*", Model: "m"}, // context_window omitted when 0
		},
	}
	out, err := AddProfileText(base, nil, pf)
	if err != nil {
		t.Fatal(err)
	}
	fc := mustLoad(t, out)
	if len(fc.Providers) != 3 || len(fc.Profiles) != 4 {
		t.Fatalf("providers=%d profiles=%d", len(fc.Providers), len(fc.Profiles))
	}
	got := profilePtr(fc, "work")
	if got == nil || got.Provider != "zai" || len(got.Models) != 2 ||
		got.Models[0].ContextWindow != 1000000 || got.Models[1].ContextWindow != 0 {
		t.Errorf("work profile = %+v", got)
	}
	// context_window omitted only for the new zero rule.
	if n := strings.Count(out, "context_window:"); n != strings.Count(base, "context_window:")+1 {
		t.Errorf("context_window lines = %d, want %d", n, strings.Count(base, "context_window:")+1)
	}
	// Round trip: removing the added profile restores the input byte-for-byte.
	back, err := RemoveProfileText(out, "work", false)
	if err != nil || back != base {
		t.Errorf("round trip: err=%v equal=%v", err, back == base)
	}
	mustLoad(t, back)
}

func TestAddProfileWithProvider(t *testing.T) {
	base := string(DefaultTemplate())
	prov := &ProviderSpec{
		Name:      "glm",
		Type:      ProviderTypeAnthropicCompat,
		BaseURL:   "https://api.example.com/v1",
		APIKeyEnv: "GLM_API_KEY",
		EffortMap: map[string]string{"high": "max", "low": "low", "medium": "high"},
	}
	pf := ProfileSpec{
		Name:     "glm",
		Provider: "glm",
		Managed:  true,
		Rules:    []ModelRule{{Match: "*", Model: "glm-5.3-flash", ContextWindow: 1000000}},
	}
	out, err := AddProfileText(base, prov, pf)
	if err != nil {
		t.Fatal(err)
	}
	begin, end := ManagedMarkers("glm")
	if !strings.Contains(out, begin) || !strings.Contains(out, end) {
		t.Error("managed markers missing")
	}
	if !HasManaged(out, "glm") {
		t.Error("HasManaged = false, want true")
	}
	fc := mustLoad(t, out)
	if len(fc.Providers) != 4 || len(fc.Profiles) != 4 {
		t.Fatalf("providers=%d profiles=%d", len(fc.Providers), len(fc.Profiles))
	}
	gp := providerPtr(fc, "glm")
	if gp == nil || gp.Type != ProviderTypeAnthropicCompat || gp.BaseURL != "https://api.example.com/v1" ||
		gp.APIKeyEnv != "GLM_API_KEY" || gp.EffortMap["low"] != "low" || gp.EffortMap["medium"] != "high" ||
		gp.EffortMap["high"] != "max" || len(gp.EffortMap) != 3 {
		t.Errorf("glm provider = %+v", gp)
	}
	if got := profilePtr(fc, "glm"); got == nil || len(got.Models) != 1 {
		t.Errorf("glm profile = %+v", got)
	}
	// Round trip removing profile and its provider.
	back, err := RemoveProfileText(out, "glm", true)
	if err != nil || back != base {
		t.Errorf("round trip: err=%v equal=%v", err, back == base)
	}
	mustLoad(t, back)
	// Without removeProvider the provider survives.
	keep, err := RemoveProfileText(out, "glm", false)
	if err != nil {
		t.Fatal(err)
	}
	fc = mustLoad(t, keep)
	if len(fc.Providers) != 4 || len(fc.Profiles) != 3 {
		t.Errorf("providers=%d profiles=%d, want 4/3", len(fc.Providers), len(fc.Profiles))
	}
}

func TestAddProfileErrors(t *testing.T) {
	base := string(DefaultTemplate())
	prov := &ProviderSpec{Name: "extra", Type: ProviderTypeCodexAPI}
	tests := []struct {
		name    string
		text    string
		prov    *ProviderSpec
		pf      ProfileSpec
		wantErr string
	}{
		{"duplicate profile", base, nil, ProfileSpec{Name: "codex", Provider: "codex"}, "already exists"},
		{"duplicate provider", base, &ProviderSpec{Name: "zai", Type: ProviderTypeCodexAPI}, ProfileSpec{Name: "n", Provider: "zai"}, "already exists"},
		{"unknown provider", base, nil, ProfileSpec{Name: "n", Provider: "nope"}, "unknown provider"},
		{"no providers list", "a: 1\n", prov, ProfileSpec{Name: "n", Provider: "p"}, "profiles: ["},
		{"no providers list, profiles ok", "profiles: [\n]\n", prov, ProfileSpec{Name: "n", Provider: "p"}, "providers: ["},
		{"no profiles list", "providers: [\n]\n", nil, ProfileSpec{Name: "n", Provider: "p"}, "profiles: ["},
		{"bad profile name", base, nil, ProfileSpec{Name: "Bad", Provider: "zai"}, "[a-z0-9_-]"},
		{"bad provider name", base, &ProviderSpec{Name: "Bad Name", Type: ProviderTypeCodexAPI}, ProfileSpec{Name: "n", Provider: "Bad Name"}, "provider name"},
		{"provider without type", base, &ProviderSpec{Name: "x"}, ProfileSpec{Name: "n", Provider: "x"}, "type is required"},
	}
	for _, tt := range tests {
		out, err := AddProfileText(tt.text, tt.prov, tt.pf)
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: err = %v, want it to mention %q", tt.name, err, tt.wantErr)
		}
		if out != tt.text {
			t.Errorf("%s: text was changed on error", tt.name)
		}
	}
}

func TestAddProfileCRLF(t *testing.T) {
	pfx := "listen.host: 127.0.0.1\r\n\r\n" +
		"providers: [\r\n    {\r\n        name: p1\r\n        type: codex_api\r\n    }\r\n]\r\n\r\n" +
		"profiles: [\r\n    {\r\n        name: a\r\n        provider: p1\r\n    }\r\n"
	in := pfx + "]\r\n"
	out, err := AddProfileText(in, nil, ProfileSpec{Name: "b", Provider: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	// A rule-less profile renders exactly 4 lines, all inserted CRLF, right
	// before the closing bracket; everything else is byte-identical.
	ins := "    {\r\n        name: b\r\n        provider: p1\r\n    }\r\n"
	want := in[:strings.LastIndex(in, "]\r\n")] + ins + "]\r\n"
	if out != want {
		t.Errorf("CRLF add:\ngot:  %q\nwant: %q", out, want)
	}
	// The real loader is fed the CRLF bytes as-is.
	fc := mustLoad(t, out)
	if len(fc.Profiles) != 2 || profilePtr(fc, "b") == nil {
		t.Errorf("profiles = %+v", fc.Profiles)
	}
	// Add then Remove round trip is byte-identical and loads.
	back, err := RemoveProfileText(out, "b", false)
	if err != nil || back != in {
		t.Errorf("CRLF round trip: err=%v equal=%v", err, back == in)
	}
	mustLoad(t, back)
	// Removing a profile from the CRLF file loads through the real loader too.
	rem, err := RemoveProfileText(out, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	fc = mustLoad(t, rem)
	if len(fc.Profiles) != 1 || profilePtr(fc, "b") == nil {
		t.Errorf("profiles = %+v", fc.Profiles)
	}
}

func TestAddRemoveMixedEndings(t *testing.T) {
	// LF-dominant file with exactly one CRLF line: inserted lines stay LF and
	// the CRLF line keeps its \r.
	mixed := "providers: [\n    {\n        name: p1\n        type: codex_api\n    }\n]\n" +
		"profiles: [\n    {\n        name: a\n        provider: p1\r\n    }\n]\n"
	out, err := AddProfileText(mixed, nil, ProfileSpec{Name: "b", Provider: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	// Build the expectation from the input: insert the new profile's lines
	// right before the closing bracket of the profiles list.
	ins := "    {\n        name: b\n        provider: p1\n    }\n"
	want := mixed[:strings.LastIndex(mixed, "]\n")] + ins + "]\n"
	if out != want {
		t.Errorf("LF-dominant add:\ngot:  %q\nwant: %q", out, want)
	}
	if !strings.Contains(out, "        provider: p1\r\n") {
		t.Error("untouched CRLF line lost its \\r")
	}
	mustLoad(t, out)
	if back, err := RemoveProfileText(out, "b", false); err != nil || back != mixed {
		t.Errorf("round trip: err=%v equal=%v", err, back == mixed)
	} else {
		mustLoad(t, back)
	}

	// CRLF-dominant file with exactly one bare-LF line: inserted lines get CRLF
	// and the LF line keeps its bare ending.
	rev := "providers: [\r\n    {\r\n        name: p1\n        type: codex_api\r\n    }\r\n]\r\n" +
		"profiles: [\r\n    {\r\n        name: a\r\n        provider: p1\r\n    }\r\n]\r\n"
	out, err = AddProfileText(rev, nil, ProfileSpec{Name: "b", Provider: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	ins = "    {\r\n        name: b\r\n        provider: p1\r\n    }\r\n"
	want = rev[:strings.LastIndex(rev, "]\r\n")] + ins + "]\r\n"
	if out != want {
		t.Errorf("CRLF-dominant add:\ngot:  %q\nwant: %q", out, want)
	}
	if !strings.Contains(out, "        name: p1\n") {
		t.Error("untouched LF line gained a \\r")
	}
	mustLoad(t, out)
	if back, err := RemoveProfileText(out, "b", false); err != nil || back != rev {
		t.Errorf("round trip: err=%v equal=%v", err, back == rev)
	} else {
		mustLoad(t, back)
	}
}

func TestAddProfileCopiesSiblingIndent(t *testing.T) {
	// Ktav takes spaces only: a tab before `{` does not parse.
	for _, indent := range []string{"  ", "        "} {
		base := "providers: [\n" + indent + "{\n" + indent + "    name: p1\n" +
			indent + "    type: codex_api\n" + indent + "}\n]\n" +
			"profiles: [\n" + indent + "{\n" + indent + "    name: a\n" +
			indent + "    provider: p1\n" + indent + "}\n]\n"
		out, err := AddProfileText(base, nil, ProfileSpec{Name: "b", Provider: "p1"})
		if err != nil {
			t.Fatalf("indent %q: %v", indent, err)
		}
		want := base[:strings.LastIndex(base, "]")] +
			indent + "{\n" + indent + "    name: b\n" + indent + "    provider: p1\n" + indent + "}\n" +
			"]\n"
		if out != want {
			t.Errorf("indent %q:\ngot:  %q\nwant: %q", indent, out, want)
		}
		fc := mustLoad(t, out)
		if len(fc.Profiles) != 2 || profilePtr(fc, "b") == nil {
			t.Errorf("indent %q: profiles = %+v", indent, fc.Profiles)
		}
	}
}

const oddSpacingSample = "providers: [\n" +
	"    {\n        name: p1\n        type: codex_api\n    }\n" +
	"    {\n        name: p2\n        type: codex_api\n    }\n" +
	"]\n" +
	"profiles: [\n" +
	"    {\n        name:  work  \n        provider:  p1\n    }\n" +
	"    {\n        name: a\n        provider:  p2  \n    }\n" +
	"]\n"

func TestOddSpacingFoundAndRemoved(t *testing.T) {
	// The oddly written profile is found: duplicate add must error.
	if _, err := AddProfileText(oddSpacingSample, nil, ProfileSpec{Name: "work", Provider: "p1"}); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want already-exists error", err)
	}
	// Removing the profile also removes its now-unused provider p1, while p2
	// (referenced with odd spacing) survives byte-identically.
	out, err := RemoveProfileText(oddSpacingSample, "work", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "        provider:  p2  \n") || strings.Contains(out, "name: p1") {
		t.Errorf("wrong objects removed:\n%s", out)
	}
	fc := mustLoad(t, out)
	if len(fc.Profiles) != 1 || profilePtr(fc, "a") == nil ||
		len(fc.Providers) != 1 || providerPtr(fc, "p2") == nil {
		t.Errorf("providers=%d profiles=%d", len(fc.Providers), len(fc.Profiles))
	}
}

func TestOddSpacingKeepsSharedProvider(t *testing.T) {
	// The SURVIVING profile carries the odd-spaced reference; the removed one
	// is plain. removeProvider must still keep p1.
	shared := "providers: [\n" +
		"    {\n        name: p1\n        type: codex_api\n    }\n" +
		"]\n" +
		"profiles: [\n" +
		"    {\n        name: work\n        provider: p1\n    }\n" +
		"    {\n        name: a\n        provider :  p1\n    }\n" +
		"]\n"
	out, err := RemoveProfileText(shared, "work", true)
	if err != nil {
		t.Fatal(err)
	}
	fc := mustLoad(t, out)
	if len(fc.Profiles) != 1 || profilePtr(fc, "a") == nil ||
		len(fc.Providers) != 1 || providerPtr(fc, "p1") == nil {
		t.Errorf("providers=%d profiles=%d", len(fc.Providers), len(fc.Profiles))
	}

	// Reverse: the REMOVED profile carries the odd-spaced reference to a
	// provider nobody else uses; removeProvider must drop it.
	rev := "providers: [\n" +
		"    {\n        name: p1\n        type: codex_api\n    }\n" +
		"    {\n        name: p3\n        type: codex_api\n    }\n" +
		"]\n" +
		"profiles: [\n" +
		"    {\n        name: work\n        provider :  p3\n    }\n" +
		"    {\n        name: a\n        provider: p1\n    }\n" +
		"]\n"
	out, err = RemoveProfileText(rev, "work", true)
	if err != nil {
		t.Fatal(err)
	}
	fc = mustLoad(t, out)
	if len(fc.Profiles) != 1 || profilePtr(fc, "a") == nil ||
		len(fc.Providers) != 1 || providerPtr(fc, "p1") == nil || providerPtr(fc, "p3") != nil {
		t.Errorf("providers=%+v profiles=%d", fc.Providers, len(fc.Profiles))
	}
}

const removeSample = `listen.host: 127.0.0.1

providers: [
    {
        name: p1
        type: codex_api
    }
    {
        name: p2
        type: codex_api
    }
]

## a comment about profiles
profiles: [
    ## first
    {
        name: a
        provider: p1
    }
    {
        name: b
        provider: p1
    }
    {
        name: c
        provider: p2
    }
]
`

func TestRemoveProfileKeepsSharedProvider(t *testing.T) {
	out, err := RemoveProfileText(removeSample, "b", true)
	if err != nil {
		t.Fatal(err)
	}
	// p1 is still used by profile a, so it must stay.
	fc := mustLoad(t, out)
	if len(fc.Profiles) != 2 || profilePtr(fc, "b") != nil ||
		len(fc.Providers) != 2 || providerPtr(fc, "p1") == nil {
		t.Errorf("providers=%d profiles=%d", len(fc.Providers), len(fc.Profiles))
	}
	if !strings.Contains(out, "## first") {
		t.Error("neighbouring comment was removed")
	}
}

func TestRemoveProfileRemovesUnusedProvider(t *testing.T) {
	out, err := RemoveProfileText(removeSample, "c", true)
	if err != nil {
		t.Fatal(err)
	}
	fc := mustLoad(t, out)
	if len(fc.Providers) != 1 || providerPtr(fc, "p2") != nil || len(fc.Profiles) != 2 {
		t.Errorf("providers=%d profiles=%d", len(fc.Providers), len(fc.Profiles))
	}
}

func TestRemoveLastProfileWithModelRules(t *testing.T) {
	// The only profile's models list nests objects: nested braces must not
	// fool the last-profile guard.
	one := "providers: [\n    {\n        name: p1\n        type: codex_api\n    }\n]\n" +
		"profiles: [\n    {\n        name: a\n        provider: p1\n" +
		"        models: [\n            {\n                match: *\n" +
		"                model: m\n            }\n        ]\n    }\n]\n"
	out, err := RemoveProfileText(one, "a", true)
	if err == nil || !strings.Contains(err.Error(), "cannot remove the last profile") {
		t.Errorf("err = %v, want last-profile error", err)
	}
	if out != one {
		t.Error("text was changed on error")
	}

	// Two profiles, one with several rules: removal must be allowed and only
	// take the target object.
	two := "providers: [\n    {\n        name: p1\n        type: codex_api\n    }\n]\n" +
		"profiles: [\n    {\n        name: a\n        provider: p1\n" +
		"        models: [\n            {\n                match: *\n" +
		"                model: m\n            }\n            {\n" +
		"                match: x*\n                model: m2\n            }\n" +
		"        ]\n    }\n    {\n        name: b\n        provider: p1\n    }\n]\n"
	out, err = RemoveProfileText(two, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	fc := mustLoad(t, out)
	if len(fc.Profiles) != 1 || profilePtr(fc, "b") == nil {
		t.Errorf("profiles = %+v", fc.Profiles)
	}
	if !strings.Contains(out, "name: b") || strings.Contains(out, "name: a") {
		t.Errorf("wrong profile removed:\n%s", out)
	}
}

func TestRemoveProfileErrors(t *testing.T) {
	one := "providers: [\n    {\n        name: p1\n        type: codex_api\n    }\n]\nprofiles: [\n    {\n        name: a\n        provider: p1\n    }\n]\n"
	out, err := RemoveProfileText(one, "a", true)
	if err == nil || !strings.Contains(err.Error(), "cannot remove the last profile") {
		t.Errorf("err = %v, want last-profile error", err)
	}
	if out != one {
		t.Error("text was changed on error")
	}
	if _, err := RemoveProfileText(removeSample, "nope", false); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want not-found error", err)
	}
}

func TestRemoveProfileManagedMarkers(t *testing.T) {
	out, err := RemoveProfileText(string(DefaultTemplate()), "codex", false)
	if err != nil {
		t.Fatal(err)
	}
	mustLoad(t, out)
	if HasManaged(out, "codex") {
		t.Error("managed markers survived removal")
	}
	if !HasManaged(out, "zai") {
		t.Error("other profile's managed block was touched")
	}
}

func TestEditFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.ktav")
	orig := string(DefaultTemplate())
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	backups := func() []string {
		m, _ := filepath.Glob(filepath.Join(dir, "*.bak-*"))
		return m
	}

	// Valid edit: backup of the original, new content loads.
	backup, err := EditFile(p, func(text string) (string, error) {
		return AddProfileText(text, nil, ProfileSpec{Name: "work", Provider: "zai"})
	})
	if err != nil || backup == "" || !strings.HasPrefix(backup, p+".bak-") {
		t.Fatalf("EditFile = %q, %v", backup, err)
	}
	if b, _ := os.ReadFile(backup); string(b) != orig {
		t.Error("backup does not hold the original content")
	}
	if fc, ferr := loadFile(p); ferr != nil || len(fc.Profiles) != 4 {
		t.Fatalf("edited file: profiles=%d err=%v", len(fc.Profiles), ferr)
	}
	if n := len(backups()); n != 1 {
		t.Fatalf("backups = %d, want 1", n)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".ccgp-edit-*")); len(m) != 0 {
		t.Errorf("temp files left behind: %v", m)
	}

	// Invalid edit: error, no backup, file untouched.
	current, _ := os.ReadFile(p)
	before := len(backups())
	if backup, err := EditFile(p, func(string) (string, error) {
		return "providers: [\n    {\n        name: bad\n        type: nope\n    }\n]\n", nil
	}); err == nil || backup != "" {
		t.Fatalf("invalid edit = %q, %v, want error", backup, err)
	}
	if b, _ := os.ReadFile(p); string(b) != string(current) {
		t.Error("file was modified by an invalid edit")
	}
	if len(backups()) != before {
		t.Error("invalid edit created a backup")
	}

	// Identity edit: nothing happens.
	if backup, err := EditFile(p, func(text string) (string, error) { return text, nil }); err != nil || backup != "" {
		t.Fatalf("identity edit = %q, %v", backup, err)
	}
	if len(backups()) != before {
		t.Error("identity edit created a backup")
	}

	// Edit errors pass through.
	if _, err := EditFile(p, func(string) (string, error) { return "", os.ErrPermission }); err == nil {
		t.Error("edit error swallowed")
	}
}

func TestIsListHeader(t *testing.T) {
	trueCases := []string{
		"profiles: [",
		"profiles:  [",
		"profiles : [",
		"\tprofiles: [\r",
		"    profiles: [",
	}
	for _, line := range trueCases {
		if !isListHeader(line, "profiles") {
			t.Errorf("isListHeader(%q, profiles) = false, want true", line)
		}
	}
	falseCases := []string{
		"profiles: []",
		"profiles [",
		"profilesX: [",
		"# profiles: [",
		"## profiles: [",
		"profiles: [ x",
		"",
	}
	for _, line := range falseCases {
		if isListHeader(line, "profiles") {
			t.Errorf("isListHeader(%q, profiles) = true, want false", line)
		}
	}
	if isListHeader("profiles: [", "providers") {
		t.Error(`isListHeader("profiles: [", providers) = true, want false`)
	}
}

func TestKeyValue(t *testing.T) {
	tests := []struct {
		line, key, val string
		ok             bool
	}{
		{"listen.host: 1", "listen.host", "1", true},
		{"name:  work  ", "name", "work", true},
		{"provider :  p1", "provider", "p1", true},
		{"provider\t: p1", "provider", "p1", true},
		{"## comment: x", "", "", false},
		{"no colon here", "", "", false},
	}
	for _, tt := range tests {
		k, v, ok := keyValue(tt.line)
		if ok != tt.ok || k != tt.key || v != tt.val {
			t.Errorf("keyValue(%q) = %q, %q, %v; want %q, %q, %v", tt.line, k, v, ok, tt.key, tt.val, tt.ok)
		}
	}
}

func TestDominantCRLF(t *testing.T) {
	crlf := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "x\r"
		}
		return out
	}
	lf := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "x"
		}
		return out
	}
	// 7 CRLF lines + 6 LF lines, all newline-terminated (split's last element
	// is the empty tail after the final "\n").
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"7 CRLF + 6 LF + trailing newline", append(append(crlf(7), lf(6)...), ""), true},
		{"6 CRLF + 7 LF + trailing newline", append(append(crlf(6), lf(7)...), ""), false},
		{"tie", append(append(crlf(6), lf(6)...), ""), false},
		{"single line no newline", []string{"x"}, false},
	}
	for _, tt := range tests {
		if got := dominantCRLF(tt.lines); got != tt.want {
			t.Errorf("%s: dominantCRLF = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// oddHeaderSample uses odd but legal header spacing (Ktav needs a space after
// the colon, so `providers :[` is not legal).
const oddHeaderSample = "listen.host: 127.0.0.1\n" +
	"providers : [\n    {\n        name: p1\n        type: codex_api\n    }\n]\n" +
	"profiles:  [\n    {\n        name: a\n        provider: p1\n    }\n]\n"

func TestOddHeaderSpacingAddAndRemove(t *testing.T) {
	out, err := AddProfileText(oddHeaderSample, nil, ProfileSpec{Name: "b", Provider: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	fc := mustLoad(t, out)
	if len(fc.Profiles) != 2 || profilePtr(fc, "b") == nil {
		t.Errorf("add: providers=%d profiles=%d", len(fc.Providers), len(fc.Profiles))
	}
	back, err := RemoveProfileText(out, "b", false)
	if err != nil || back != oddHeaderSample {
		t.Errorf("remove: err=%v equal=%v", err, back == oddHeaderSample)
	}
	mustLoad(t, back)
}

func TestOddProviderSpacingFoundRemovedCounted(t *testing.T) {
	src := "providers: [\n    {\n        name: p9\n        type: codex_api\n    }\n" +
		"    {\n        name: p1\n        type: codex_api\n    }\n]\n" +
		"profiles: [\n    {\n        name: a\n        provider : p9\n    }\n" +
		"    {\n        name: b\n        provider: p1\n    }\n]\n"
	// Found: duplicate add errors.
	if _, err := AddProfileText(src, nil, ProfileSpec{Name: "a", Provider: "p1"}); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want already-exists", err)
	}
	// Removed, and p9 (only referenced with odd spacing) is dropped.
	out, err := RemoveProfileText(src, "a", true)
	if err != nil {
		t.Fatal(err)
	}
	fc := mustLoad(t, out)
	if len(fc.Profiles) != 1 || profilePtr(fc, "b") == nil ||
		len(fc.Providers) != 1 || providerPtr(fc, "p9") != nil || providerPtr(fc, "p1") == nil {
		t.Errorf("providers=%+v profiles=%d", fc.Providers, len(fc.Profiles))
	}
}

func TestCRLFAddRemoveRoundTrip(t *testing.T) {
	in := "listen.host: 127.0.0.1\r\n" +
		"providers: [\r\n    {\r\n        name: p1\r\n        type: codex_api\r\n    }\r\n]\r\n" +
		"profiles: [\r\n    {\r\n        name: a\r\n        provider: p1\r\n    }\r\n]\r\n"
	out, err := AddProfileText(in, nil, ProfileSpec{Name: "b", Provider: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	fc := mustLoad(t, out)
	if len(fc.Profiles) != 2 || profilePtr(fc, "b") == nil {
		t.Errorf("add: profiles=%d", len(fc.Profiles))
	}
	back, err := RemoveProfileText(out, "b", false)
	if err != nil || back != in {
		t.Errorf("round trip: err=%v identical=%v", err, back == in)
	}
	mustLoad(t, back)
	// Removing a pre-existing profile from the CRLF file loads too.
	rem, err := RemoveProfileText(out, "a", true)
	if err != nil {
		t.Fatal(err)
	}
	fc = mustLoad(t, rem)
	if len(fc.Profiles) != 1 || profilePtr(fc, "b") == nil ||
		len(fc.Providers) != 1 || providerPtr(fc, "p1") == nil {
		t.Errorf("remove: providers=%d profiles=%d", len(fc.Providers), len(fc.Profiles))
	}
}

// Two edits in the same second must not overwrite the first backup, which
// holds the user's original file.
func TestEditFileKeepsEveryBackup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.ktav")
	orig := string(DefaultTemplate())
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := EditFile(p, func(text string) (string, error) {
		return AddProfileText(text, nil, ProfileSpec{Name: "one", Provider: "zai"})
	})
	if err != nil {
		t.Fatal(err)
	}
	afterFirst, _ := os.ReadFile(p)
	second, err := EditFile(p, func(text string) (string, error) {
		return AddProfileText(text, nil, ProfileSpec{Name: "two", Provider: "zai"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("both edits used the backup %q", first)
	}
	if b, _ := os.ReadFile(first); string(b) != orig {
		t.Error("first backup no longer holds the original")
	}
	if b, _ := os.ReadFile(second); string(b) != string(afterFirst) {
		t.Error("second backup does not hold the state before the second edit")
	}
}
