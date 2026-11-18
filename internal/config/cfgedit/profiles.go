package cfgedit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgtext"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/securefile"
)

// profileNameRe allows 1-32 chars: lowercase letters, digits, '-' and '_',
// not starting with '-' or '_'.
var profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidateProfileName rejects names that cannot be used as `ccgp <name>`:
// malformed or reserved (claude is the ccgp --send endpoint name).
func ValidateProfileName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("profile name is empty")
	case !profileNameRe.MatchString(name):
		return fmt.Errorf("profile name %q: use 1-32 characters [a-z0-9_-], starting with a letter or digit", name)
	case config.ReservedProfile(name):
		return fmt.Errorf("profile name %q is reserved: claude is the ccgp --send endpoint name", name)
	}
	return nil
}

// NextFreeName returns base when free, else stem2, stem3, ... not in taken,
// where stem is base with trailing digits stripped (all-digit names keep base
// as the stem, since stripping would leave it empty).
func NextFreeName(base string, taken func(string) bool) string {
	if !taken(base) {
		return base
	}
	stem := strings.TrimRight(base, "0123456789")
	if stem == "" {
		stem = base
	}
	for n := 2; ; n++ {
		name := stem + strconv.Itoa(n)
		if !taken(name) {
			return name
		}
	}
}

// ProviderSpec and ProfileSpec describe what AddProfileText inserts.
type ProviderSpec struct {
	Name      string
	Type      string
	BaseURL   string
	APIKeyEnv string
	// EffortMap renders as a nested object with sorted keys.
	EffortMap map[string]string
	// Peak and PeakTZ are the provider's peak hours (see SetProviderPeakText).
	Peak   []peak.Window
	PeakTZ string
	// Proxy is the provider's proxy: a URL or config.ProxyNone ("": the global one).
	Proxy string
}

type ProfileSpec struct {
	Name     string
	Provider string
	Rules    []config.ModelRule
	Managed  bool
}

// renderProvider renders a provider object in config.example.ktav style.
func renderProvider(indent string, p *ProviderSpec) []string {
	lines := []string{indent + "{"}
	add := func(k, v string) { lines = append(lines, indent+"    "+k+": "+v) }
	add("name", p.Name)
	add("type", p.Type)
	if p.BaseURL != "" {
		add("base_url", p.BaseURL)
	}
	if p.APIKeyEnv != "" {
		add("api_key_env", p.APIKeyEnv)
	}
	if p.Proxy != "" {
		add("proxy", p.Proxy)
	}
	if len(p.EffortMap) > 0 {
		keys := make([]string, 0, len(p.EffortMap))
		for k := range p.EffortMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lines = append(lines, indent+"    effort_map: {")
		for _, k := range keys {
			lines = append(lines, indent+"        "+k+": "+p.EffortMap[k])
		}
		lines = append(lines, indent+"    }")
	}
	lines = append(lines, renderPeak(indent+"    ", p.Peak, p.PeakTZ)...)
	return append(lines, indent+"}")
}

// renderProfile renders a profile object; managed markers wrap the models list.
func renderProfile(indent string, pf ProfileSpec) []string {
	lines := []string{indent + "{", indent + "    name: " + pf.Name, indent + "    provider: " + pf.Provider}
	if len(pf.Rules) > 0 {
		begin, end := ManagedMarkers(pf.Name)
		if pf.Managed {
			lines = append(lines, indent+"    "+begin)
		}
		lines = append(lines, indent+"    models: [")
		for _, r := range pf.Rules {
			lines = append(lines, indent+"        {",
				indent+"            match: "+r.Match,
				indent+"            model: "+r.Model)
			if r.ContextWindow > 0 {
				lines = append(lines, indent+"            context_window: "+strconv.FormatInt(r.ContextWindow, 10))
			}
			lines = append(lines, indent+"        }")
		}
		lines = append(lines, indent+"    ]")
		if pf.Managed {
			lines = append(lines, indent+"    "+end)
		}
	}
	return append(lines, indent+"}")
}

// AddProfileText appends the provider (when prov is set) and the profile to
// the config text. Everything outside the inserted blocks stays byte-identical.
func AddProfileText(text string, prov *ProviderSpec, pf ProfileSpec) (string, error) {
	if err := ValidateProfileName(pf.Name); err != nil {
		return text, err
	}
	if prov != nil {
		if err := ValidateProfileName(prov.Name); err != nil {
			return text, fmt.Errorf("provider name: %w", err)
		}
		if prov.Type == "" {
			return text, fmt.Errorf("provider %q: type is required", prov.Name)
		}
	}
	lines := strings.Split(text, "\n")
	crlf := cfgtext.DominantCRLF(lines)
	pfs, err := cfgtext.FindList(lines, "profiles")
	if err != nil {
		return text, err
	}
	if _, _, ok := cfgtext.FindObjectNamed(lines, pfs.Open, pfs.Close, pf.Name, true); ok {
		return text, fmt.Errorf("profile %q already exists in the config", pf.Name)
	}
	ins := map[int][]string{}
	if prov != nil {
		prs, err := cfgtext.FindList(lines, "providers")
		if err != nil {
			return text, err
		}
		if _, _, ok := cfgtext.FindObjectNamed(lines, prs.Open, prs.Close, prov.Name, false); ok {
			return text, fmt.Errorf("provider %q already exists in the config", prov.Name)
		}
		ins[prs.Close] = cfgtext.WithEndings(renderProvider(cfgtext.SiblingIndent(lines, prs), prov), crlf)
	}
	if prov == nil || pf.Provider != prov.Name {
		if _, _, ok := cfgtext.FindObjectNamed(lines, 0, len(lines), pf.Provider, false); !ok {
			return text, fmt.Errorf("profile %q: unknown provider %q", pf.Name, pf.Provider)
		}
	}
	ins[pfs.Close] = cfgtext.WithEndings(renderProfile(cfgtext.SiblingIndent(lines, pfs), pf), crlf)
	var out []string
	for i, l := range lines {
		if block, ok := ins[i]; ok {
			out = append(out, block...)
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), nil
}

// RemoveProfileText deletes a profile object (and, when removeProvider and no
// other profile uses it, its provider object). Comments and blank lines
// outside the removed objects stay untouched.
func RemoveProfileText(text, profile string, removeProvider bool) (string, error) {
	lines := strings.Split(text, "\n")
	pfs, err := cfgtext.FindList(lines, "profiles")
	if err != nil {
		return text, err
	}
	o, c, provider, err := cfgtext.FindProfileObject(lines, profile)
	if err != nil {
		return text, err
	}
	// Only top-level objects count: nested model-rule objects do not keep a
	// profile alive.
	objects, depth := 0, 0
	for i := pfs.Open + 1; i < pfs.Close; i++ {
		switch cfgtext.TrimLine(lines[i]) {
		case "{":
			if depth == 0 {
				objects++
			}
			depth++
		case "}":
			depth--
		}
	}
	if objects <= 1 {
		return text, fmt.Errorf("cannot remove the last profile %q", profile)
	}
	drop := map[int]bool{}
	for i := o; i <= c; i++ {
		drop[i] = true
	}
	if removeProvider {
		refs := 0
		for i := pfs.Open + 1; i < pfs.Close; i++ {
			if !drop[i] {
				if k, v, ok := cfgtext.KeyValue(lines[i]); ok && k == "provider" && v == provider {
					refs++
				}
			}
		}
		if refs == 0 {
			if prs, err := cfgtext.FindList(lines, "providers"); err == nil {
				if po, pc, ok := cfgtext.FindObjectNamed(lines, prs.Open, prs.Close, provider, false); ok {
					for i := po; i <= pc; i++ {
						drop[i] = true
					}
				}
			}
		}
	}
	var out []string
	for i, l := range lines {
		if !drop[i] {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n"), nil
}

// EditFile applies edit to the config file, validating the result with the
// real loader before touching the original. The original is kept as
// <path>.bak-<timestamp>; the backup path is returned ("" when edit was a
// no-op).
func EditFile(path string, edit func(string) (string, error)) (backup string, err error) {
	old, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	out, err := edit(string(old))
	if err != nil {
		return "", err
	}
	if out == string(old) {
		return "", nil
	}
	// Validate with the real loader on a temp file next to the original.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ccgp-edit-*.ktav")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(out); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if _, err := config.LoadFile(tmpName); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := os.Remove(tmpName); err != nil {
		return "", err
	}
	backup, err = config.BackupFile(path)
	if err != nil {
		return "", err
	}
	return backup, securefile.WriteFile(path, []byte(out))
}
