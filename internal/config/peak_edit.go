package config

import (
	"fmt"
	"strings"
)

// renderPeak renders a provider's peak settings in config.example.ktav style:
// the zone line, then the windows as objects (days is left out when empty).
func renderPeak(indent string, windows []PeakWindow, tz string) []string {
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
func SetProviderPeakText(text, provider string, windows []PeakWindow, tz string) (string, error) {
	if len(windows) == 0 {
		tz = ""
	} else if _, err := NewPeakSchedule(windows, tz); err != nil {
		return "", err
	}
	lines := strings.Split(text, "\n")
	prs, err := findList(lines, "providers")
	if err != nil {
		return "", err
	}
	open, closeAt, ok := findObjectNamed(lines, prs.open, prs.close, provider, false)
	if !ok {
		return "", fmt.Errorf("provider %q not found in the config", provider)
	}
	child := indentOf(lines[open]) + "    "
	for i := open + 1; i < closeAt; i++ {
		if _, _, ok := keyValue(lines[i]); ok {
			child = indentOf(lines[i])
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
		if i > open && i < closeAt && indentOf(l) == child {
			if k, _, ok := keyValue(l); ok && k == "peak_tz" {
				continue
			}
			if isListHeader(l, "peak") {
				end := i
				for ; end < closeAt; end++ {
					if isCloseBracket(lines[end]) && indentOf(lines[end]) == child {
						break
					}
				}
				i = end
				continue
			}
		}
		kept = append(kept, l)
		if i > open && i < closeAt && indentOf(l) == child {
			if k, _, ok := keyValue(l); ok && (k == "name" || k == "type" || k == "base_url" || k == "api_key_env") {
				insertAt = len(kept)
			}
		}
	}
	add := withEndings(renderPeak(child, windows, tz), dominantCRLF(lines))
	if len(add) == 0 {
		return strings.Join(kept, "\n"), nil
	}
	if insertAt < 0 {
		insertAt = closeKept // none of the scalar keys is at the object's own indent
	}
	out := append(append(append([]string{}, kept[:insertAt]...), add...), kept[insertAt:]...)
	return strings.Join(out, "\n"), nil
}
