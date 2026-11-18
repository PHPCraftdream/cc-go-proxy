package config

import (
	"fmt"
	"strings"
)

const (
	SpeedFast     = "fast"
	SpeedStandard = "standard"
)

func validSpeedFamily(family string) bool {
	if family == "" {
		return false
	}
	for _, r := range family {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// SpeedFor returns the configured speed for a logical family; empty means inherit.
func (p *Profile) SpeedFor(family string) string {
	if p == nil {
		return ""
	}
	return p.SpeedRules[family]
}

// EffectiveFast applies a family rule without changing the requested model.
func (p *Profile) EffectiveFast(family string, clientFast bool) bool {
	switch p.SpeedFor(family) {
	case SpeedFast:
		return true
	case SpeedStandard:
		return false
	default:
		return clientFast
	}
}

// SetProfileSpeedText edits one family rule; empty speed removes it.
// Use with EditFile to validate and persist the resulting config.
func SetProfileSpeedText(text, profile, family, speed string) (string, error) {
	if !validSpeedFamily(family) {
		return "", fmt.Errorf("invalid speed family %q", family)
	}
	if speed != "" && speed != SpeedFast && speed != SpeedStandard {
		return "", fmt.Errorf("invalid speed %q", speed)
	}
	lines := strings.Split(text, "\n")
	open, end, _, err := findProfileObject(lines, profile)
	if err != nil {
		return "", err
	}
	child := indentOf(lines[open]) + "    "
	for i := open + 1; i < end; i++ {
		if _, _, ok := keyValue(lines[i]); ok {
			child = indentOf(lines[i])
			break
		}
	}
	for i := open + 1; i < end; i++ {
		if indentOf(lines[i]) != child {
			continue
		}
		k, v, ok := keyValue(lines[i])
		if !ok || k != "speed_rules" {
			continue
		}
		if strings.TrimSpace(v) != "{" {
			return "", fmt.Errorf("profile %q: speed_rules must be a multiline object", profile)
		}
		for j := i + 1; j < end; j++ {
			if strings.TrimSpace(lines[j]) == "}" && indentOf(lines[j]) == child {
				if speed == "" {
					at, other := -1, false
					for n := i + 1; n < j; n++ {
						line := strings.TrimSpace(lines[n])
						if line == "" || strings.HasPrefix(line, "##") {
							continue
						}
						k, _, ok := keyValue(lines[n])
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
				return setScalarKey(lines, i, j, family, speed, nil)
			}
		}
		return "", fmt.Errorf("profile %q: unclosed speed_rules", profile)
	}
	if speed == "" {
		return text, nil
	}
	add := withEndings([]string{child + "speed_rules: {", child + "    " + family + ": " + speed, child + "}"}, dominantCRLF(lines))
	out := append(append(append([]string{}, lines[:end]...), add...), lines[end:]...)
	return strings.Join(out, "\n"), nil
}
