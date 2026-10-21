package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/securefile"
)

// profileNameRe allows 1-32 chars: lowercase letters, digits, '-' and '_',
// not starting with '-' or '_'.
var profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidateProfileName rejects names that cannot be used as `ccgp <name>`:
// malformed or colliding with a ccgp command.
func ValidateProfileName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("profile name is empty")
	case !profileNameRe.MatchString(name):
		return fmt.Errorf("profile name %q: use 1-32 characters [a-z0-9_-], starting with a letter or digit", name)
	case reservedProfiles[name]:
		return fmt.Errorf("profile name %q is a ccgp command name", name)
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
}

type ProfileSpec struct {
	Name     string
	Provider string
	Rules    []ModelRule
	Managed  bool
}

// trimLine strips spaces, tabs and a trailing CR from a raw line so that
// recognition works for LF, CRLF and mixed files alike.
func trimLine(s string) string { return strings.Trim(s, " \t\r") }

// keyValue splits a line at its first ':' and trims spaces, tabs and '\r'
// from both sides. Comment lines never match.
func keyValue(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	key = trimLine(line[:i])
	if key == "" || strings.HasPrefix(key, "#") {
		return "", "", false
	}
	return key, trimLine(line[i+1:]), true
}

// isListHeader reports whether line is `key` + optional spaces + ':' +
// optional spaces + '[', with nothing after the bracket but spaces, tabs or
// a trailing CR. Comment lines never match (their text does not start with
// the key).
func isListHeader(line, key string) bool {
	rest, ok := strings.CutPrefix(trimLine(line), key)
	if !ok {
		return false
	}
	rest, ok = strings.CutPrefix(strings.TrimLeft(rest, " \t"), ":")
	if !ok {
		return false
	}
	return trimLine(rest) == "["
}

// isCloseBracket reports whether line is a lone ']' (with optional spaces,
// tabs and trailing CR).
func isCloseBracket(line string) bool { return trimLine(line) == "]" }

// dominantCRLF reports whether lines (split on "\n") are CRLF-dominant: a
// majority of newline-terminated elements carry their own trailing "\r".
// Only lines[:len(lines)-1] are terminated (strings.Split drops the final
// newline into the last element, which may be empty). Ties go to LF.
func dominantCRLF(lines []string) bool {
	if len(lines) < 2 {
		return false
	}
	term := lines[:len(lines)-1]
	crlf := 0
	for _, l := range term {
		if strings.HasSuffix(l, "\r") {
			crlf++
		}
	}
	return crlf*2 > len(term)
}

// withEndings appends "\r" to every inserted line for CRLF-dominant files.
// Lines the caller kept from the input are never passed here, so their own
// endings stay byte-identical.
func withEndings(lines []string, crlf bool) []string {
	if !crlf {
		return lines
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l + "\r"
	}
	return out
}

func indentOf(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }

// listSpan locates a top-level `key: [ ... ]` list, braces alone on their lines.
type listSpan struct{ open, close int }

func findList(lines []string, key string) (listSpan, error) {
	for i, l := range lines {
		if !isListHeader(l, key) {
			continue
		}
		ind := indentOf(l)
		for j := i + 1; j < len(lines); j++ {
			if isCloseBracket(lines[j]) && indentOf(lines[j]) == ind {
				return listSpan{i, j}, nil
			}
		}
		return listSpan{}, fmt.Errorf("config has no closing %q for %q", "]", key+": [")
	}
	return listSpan{}, fmt.Errorf("config has no %q list", key+": [")
}

// siblingIndent is the indent of the first object in the list; 4 spaces when
// the list is empty (the default file's style).
func siblingIndent(lines []string, sp listSpan) string {
	for i := sp.open + 1; i < sp.close; i++ {
		if strings.TrimSpace(lines[i]) == "{" {
			return indentOf(lines[i])
		}
	}
	return "    "
}

// hasKey reports whether any line's keyValue key equals key; comments never
// match.
func hasKey(lines []string, key string) bool {
	for _, l := range lines {
		if k, _, ok := keyValue(l); ok && k == key {
			return true
		}
	}
	return false
}

// findObjectNamed finds the object in [lo, hi] whose `name:` line is name.
// wantProviderKey distinguishes profiles (have a `provider` key) from
// providers.
func findObjectNamed(lines []string, lo, hi int, name string, wantProviderKey bool) (open, closeAt int, ok bool) {
	for i := lo + 1; i < hi; i++ {
		k, v, ok := keyValue(lines[i])
		if !ok || k != "name" || v != name {
			continue
		}
		o, c, found := objectAround(lines, i)
		if !found || o < lo || c > hi || hasKey(lines[o+1:c], "provider") != wantProviderKey {
			continue
		}
		return o, c, true
	}
	return 0, 0, false
}

// findProfileObject locates a profile and reads its `provider:` value.
func findProfileObject(lines []string, name string) (open, closeAt int, provider string, err error) {
	for i, l := range lines {
		k, v, ok := keyValue(l)
		if !ok || k != "name" || v != name {
			continue
		}
		o, c, ok := objectAround(lines, i)
		if !ok || !hasKey(lines[o+1:c], "provider") {
			continue
		}
		for _, l2 := range lines[o+1 : c] {
			if k, v, ok := keyValue(l2); ok && k == "provider" {
				provider = v
			}
		}
		return o, c, provider, nil
	}
	return 0, 0, "", fmt.Errorf("profile %q not found in the config", name)
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
	crlf := dominantCRLF(lines)
	pfs, err := findList(lines, "profiles")
	if err != nil {
		return text, err
	}
	if _, _, ok := findObjectNamed(lines, pfs.open, pfs.close, pf.Name, true); ok {
		return text, fmt.Errorf("profile %q already exists in the config", pf.Name)
	}
	ins := map[int][]string{}
	if prov != nil {
		prs, err := findList(lines, "providers")
		if err != nil {
			return text, err
		}
		if _, _, ok := findObjectNamed(lines, prs.open, prs.close, prov.Name, false); ok {
			return text, fmt.Errorf("provider %q already exists in the config", prov.Name)
		}
		ins[prs.close] = withEndings(renderProvider(siblingIndent(lines, prs), prov), crlf)
	}
	if prov == nil || pf.Provider != prov.Name {
		if _, _, ok := findObjectNamed(lines, 0, len(lines), pf.Provider, false); !ok {
			return text, fmt.Errorf("profile %q: unknown provider %q", pf.Name, pf.Provider)
		}
	}
	ins[pfs.close] = withEndings(renderProfile(siblingIndent(lines, pfs), pf), crlf)
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
	pfs, err := findList(lines, "profiles")
	if err != nil {
		return text, err
	}
	o, c, provider, err := findProfileObject(lines, profile)
	if err != nil {
		return text, err
	}
	// Only top-level objects count: nested model-rule objects do not keep a
	// profile alive.
	objects, depth := 0, 0
	for i := pfs.open + 1; i < pfs.close; i++ {
		switch trimLine(lines[i]) {
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
		for i := pfs.open + 1; i < pfs.close; i++ {
			if !drop[i] {
				if k, v, ok := keyValue(lines[i]); ok && k == "provider" && v == provider {
					refs++
				}
			}
		}
		if refs == 0 {
			if prs, err := findList(lines, "providers"); err == nil {
				if po, pc, ok := findObjectNamed(lines, prs.open, prs.close, provider, false); ok {
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
	if _, err := loadFile(tmpName); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := os.Remove(tmpName); err != nil {
		return "", err
	}
	backup, err = backupFile(path)
	if err != nil {
		return "", err
	}
	return backup, securefile.WriteFile(path, []byte(out))
}
