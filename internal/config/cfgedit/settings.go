package cfgedit

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgtext"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

// SetConnectionsText sets connections.<key> (active or total) in config text:
// the `connections.<key>:` line is rewritten, or added after the listen and
// connections settings (at the top when there are none). A `connections: {` block must be
// edited by hand.
func SetConnectionsText(text, key string, value int64) (string, error) {
	if key != "active" && key != "total" {
		return "", fmt.Errorf("unknown connections setting %q (active or total)", key)
	}
	return setConnectionsLine(text, key, strconv.FormatInt(value, 10))
}

// SetConnectionsProxyText sets connections.proxy in config text like
// SetConnectionsText; an empty proxy removes the line.
func SetConnectionsProxyText(text, proxy string) (string, error) {
	if proxy == "" {
		lines := strings.Split(text, "\n")
		out := lines[:0]
		for _, l := range lines {
			if !proxyLine.MatchString(strings.TrimRight(l, "\r")) {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n"), nil
	}
	if _, err := proxydial.Parse(proxy); err != nil {
		return "", err
	}
	return setConnectionsLine(text, "proxy", proxy)
}

func setConnectionsLine(text, key, value string) (string, error) {
	lines := strings.Split(text, "\n")
	line := "connections." + key + ": " + value
	re := regexp.MustCompile(`^connections\.` + key + `\s*:`)
	lastListen, block := -1, false
	for i, l := range lines {
		trimmed := strings.TrimRight(l, "\r")
		switch {
		case re.MatchString(trimmed):
			lines[i] = line + l[len(trimmed):]
			return strings.Join(lines, "\n"), nil
		case strings.HasPrefix(trimmed, "listen."), strings.HasPrefix(trimmed, "connections."):
			lastListen = i
		case connectionsBlock.MatchString(trimmed):
			block = true
		}
	}
	if block {
		return "", fmt.Errorf("config has a `connections:` block; set %s there by hand", key)
	}
	if strings.HasSuffix(lines[0], "\r") {
		line += "\r"
	}
	at := lastListen + 1
	if lastListen < 0 {
		// After the leading comment lines.
		for at < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[at]), "#") {
			at++
		}
	}
	out := append([]string{}, lines[:at]...)
	out = append(out, line)
	return strings.Join(append(out, lines[at:]...), "\n"), nil
}

var (
	connectionsBlock = regexp.MustCompile(`^connections\s*:`)
	proxyLine        = regexp.MustCompile(`^connections\.proxy\s*:`)
)

// SetProfileProxyText sets proxy in the profile's object in config text: a
// proxy URL, config.ProxyNone, or "" to remove the line (the profile inherits the
// global proxy again). The line goes after the profile's provider line.
func SetProfileProxyText(text, profile, proxy string) (string, error) {
	if proxy != "" && proxy != config.ProxyNone {
		if _, err := proxydial.Parse(proxy); err != nil {
			return "", err
		}
	}
	lines := strings.Split(text, "\n")
	open, closeAt, _, err := cfgtext.FindProfileObject(lines, profile)
	if err != nil {
		return "", err
	}
	return cfgtext.SetScalarKey(lines, open, closeAt, "proxy", proxy, []string{"provider"})
}

// SetProfileSpeedText edits one family rule; empty speed removes it.
// Use with EditFile to validate and persist the resulting config.
func SetProfileSpeedText(text, profile, family, speed string) (string, error) {
	if !config.ValidSpeedFamily(family) {
		return "", fmt.Errorf("invalid speed family %q", family)
	}
	if speed != "" && speed != config.SpeedFast && speed != config.SpeedStandard {
		return "", fmt.Errorf("invalid speed %q", speed)
	}
	lines := strings.Split(text, "\n")
	open, end, _, err := cfgtext.FindProfileObject(lines, profile)
	if err != nil {
		return "", err
	}
	child := cfgtext.IndentOf(lines[open]) + "    "
	for i := open + 1; i < end; i++ {
		if _, _, ok := cfgtext.KeyValue(lines[i]); ok {
			child = cfgtext.IndentOf(lines[i])
			break
		}
	}
	for i := open + 1; i < end; i++ {
		if cfgtext.IndentOf(lines[i]) != child {
			continue
		}
		k, v, ok := cfgtext.KeyValue(lines[i])
		if !ok || k != "speed_rules" {
			continue
		}
		if strings.TrimSpace(v) != "{" {
			return "", fmt.Errorf("profile %q: speed_rules must be a multiline object", profile)
		}
		for j := i + 1; j < end; j++ {
			if strings.TrimSpace(lines[j]) == "}" && cfgtext.IndentOf(lines[j]) == child {
				if speed == "" {
					at, other := -1, false
					for n := i + 1; n < j; n++ {
						line := strings.TrimSpace(lines[n])
						if line == "" || strings.HasPrefix(line, "##") {
							continue
						}
						k, _, ok := cfgtext.KeyValue(lines[n])
						if ok && k == family {
							at = n
						} else {
							other = true
						}
					}
					if at >= 0 && !other {
						out := append([]string{}, lines[:i]...)
						out = append(out, lines[i+1:at]...)
						out = append(out, lines[at+1:j]...)
						out = append(out, lines[j+1:]...)
						return strings.Join(out, "\n"), nil
					}
				}
				return cfgtext.SetScalarKey(lines, i, j, family, speed, nil)
			}
		}
		return "", fmt.Errorf("profile %q: unclosed speed_rules", profile)
	}
	if speed == "" {
		return text, nil
	}
	add := cfgtext.WithEndings([]string{child + "speed_rules: {", child + "    " + family + ": " + speed, child + "}"}, cfgtext.DominantCRLF(lines))
	out := append(append(append([]string{}, lines[:end]...), add...), lines[end:]...)
	return strings.Join(out, "\n"), nil
}

var (
	clientVersionLine = regexp.MustCompile(`^codex\.client_version\s*::?`)
	codexBlockLine    = regexp.MustCompile(`^codex\s*:`)
)

// CodexClientVersionText returns the `codex.client_version` value in config text.
func CodexClientVersionText(text string) (string, bool) {
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimRight(l, "\r")
		if loc := clientVersionLine.FindStringIndex(l); loc != nil {
			return strings.TrimSpace(l[loc[1]:]), true
		}
	}
	return "", false
}

// SetCodexClientVersionText writes `codex.client_version:: v` into config text:
// the line is rewritten, or added after the listen and connections settings.
// Only raise: a v not newer than the current value leaves the text unchanged.
func SetCodexClientVersionText(text, v string) (string, error) {
	if !config.ValidClientVersion(v) {
		return "", fmt.Errorf("invalid Codex client version %q (MAJOR.MINOR.PATCH)", v)
	}
	if cur, ok := CodexClientVersionText(text); ok && config.ValidClientVersion(cur) && config.CompareClientVersions(v, cur) <= 0 {
		return text, nil
	}
	lines := strings.Split(text, "\n")
	// `::` keeps the value a string whatever it looks like.
	line := "codex.client_version:: " + v
	last := -1
	for i, l := range lines {
		trimmed := strings.TrimRight(l, "\r")
		switch {
		case clientVersionLine.MatchString(trimmed):
			lines[i] = line + l[len(trimmed):]
			return strings.Join(lines, "\n"), nil
		case codexBlockLine.MatchString(trimmed):
			return "", fmt.Errorf("config has a `codex:` block; set client_version there by hand")
		case strings.HasPrefix(trimmed, "listen."), strings.HasPrefix(trimmed, "connections."):
			last = i
		}
	}
	if strings.HasSuffix(lines[0], "\r") {
		line += "\r"
	}
	at := last + 1
	if last < 0 {
		for at < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[at]), "#") {
			at++
		}
	}
	out := append([]string{}, lines[:at]...)
	out = append(out, line)
	return strings.Join(append(out, lines[at:]...), "\n"), nil
}

// RaiseCodexClientVersion raises `codex.client_version` in the config file to v
// (never lowers it). It returns the version the file held before ("" when it
// had none) and whether the file changed.
func RaiseCodexClientVersion(path, v string) (from string, changed bool, err error) {
	backup, err := EditFile(path, func(text string) (string, error) {
		from, _ = CodexClientVersionText(text)
		return SetCodexClientVersionText(text, v)
	})
	return from, backup != "", err
}

// SeedCodexClientVersion writes the built-in default into a config that has a
// codex provider but no `codex.client_version` yet; from then on the config's
// value is the one used. A no-op in env-only mode.
func SeedCodexClientVersion(cfg *config.Config) (bool, error) {
	if cfg == nil || cfg.IsEnvMode() || cfg.CodexClientVersionSet != "" || !cfg.HasCodexProvider() {
		return false, nil
	}
	_, changed, err := RaiseCodexClientVersion(cfg.ConfigFile, config.DefaultCodexClientVersion)
	if err == nil {
		cfg.CodexClientVersionSet = config.DefaultCodexClientVersion
	}
	return changed, err
}

// renderPeak renders a provider's peak settings in config.example.ktav style:
// the zone line, then the windows as objects (days is left out when empty).
func renderPeak(indent string, windows []peak.Window, tz string) []string {
	if len(windows) == 0 {
		return nil
	}
	var lines []string
	if tz != "" {
		lines = append(lines, indent+"peak_tz: "+tz)
	}
	lines = append(lines, indent+"peak: [")
	for _, w := range windows {
		lines = append(lines, indent+"    {")
		if w.Days != "" {
			lines = append(lines, indent+"        days: "+w.Days)
		}
		lines = append(lines, indent+"        from: "+w.From, indent+"        to: "+w.To, indent+"    }")
	}
	return append(lines, indent+"]")
}

// SetProviderPeakText sets the peak hours of a provider in config text: its
// `peak_tz` line and `peak` list are replaced (or added after the provider's
// name, type, base_url and api_key_env lines), and removed when windows is
// empty. Comments and everything else stay as they are.
func SetProviderPeakText(text, provider string, windows []peak.Window, tz string) (string, error) {
	if len(windows) == 0 {
		tz = ""
	} else if _, err := peak.NewSchedule(windows, tz); err != nil {
		return "", err
	}
	lines := strings.Split(text, "\n")
	prs, err := cfgtext.FindList(lines, "providers")
	if err != nil {
		return "", err
	}
	open, closeAt, ok := cfgtext.FindObjectNamed(lines, prs.Open, prs.Close, provider, false)
	if !ok {
		return "", fmt.Errorf("provider %q not found in the config", provider)
	}
	child := cfgtext.IndentOf(lines[open]) + "    "
	for i := open + 1; i < closeAt; i++ {
		if _, _, ok := cfgtext.KeyValue(lines[i]); ok {
			child = cfgtext.IndentOf(lines[i])
			break
		}
	}

	// Drop the old settings; remember where the scalar keys end.
	var kept []string
	insertAt := -1 // index into kept: after the last name/type/base_url/api_key_env line
	closeKept := 0 // index into kept of the provider's closing brace
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if i == closeAt {
			closeKept = len(kept)
		}
		if i > open && i < closeAt && cfgtext.IndentOf(l) == child {
			if k, _, ok := cfgtext.KeyValue(l); ok && k == "peak_tz" {
				continue
			}
			if cfgtext.IsListHeader(l, "peak") {
				end := i
				for ; end < closeAt; end++ {
					if cfgtext.IsCloseBracket(lines[end]) && cfgtext.IndentOf(lines[end]) == child {
						break
					}
				}
				i = end
				continue
			}
		}
		kept = append(kept, l)
		if i > open && i < closeAt && cfgtext.IndentOf(l) == child {
			if k, _, ok := cfgtext.KeyValue(l); ok && (k == "name" || k == "type" || k == "base_url" || k == "api_key_env") {
				insertAt = len(kept)
			}
		}
	}
	add := cfgtext.WithEndings(renderPeak(child, windows, tz), cfgtext.DominantCRLF(lines))
	if len(add) == 0 {
		return strings.Join(kept, "\n"), nil
	}
	if insertAt < 0 {
		insertAt = closeKept // none of the scalar keys is at the object's own indent
	}
	out := append(append(append([]string{}, kept[:insertAt]...), add...), kept[insertAt:]...)
	return strings.Join(out, "\n"), nil
}
