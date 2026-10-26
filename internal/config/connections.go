package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
)

// Upstream connections per backend: connections.active take new requests,
// connections.total caps them together with the ones still finishing older
// requests (see internal/connpool).
const (
	DefaultConnectionsActive = connpool.DefaultActive
	DefaultConnectionsTotal  = connpool.DefaultTotal
	maxConnections           = 64
)

func validateConnections(active, total int64) error {
	if active < 0 || active > maxConnections {
		return fmt.Errorf("connections.active must be 0-%d (0: default %d)", maxConnections, DefaultConnectionsActive)
	}
	if total < 0 || total > maxConnections {
		return fmt.Errorf("connections.total must be 0-%d (0: default %d)", maxConnections, DefaultConnectionsTotal)
	}
	if a, t := effectiveConnections(active, total); t < a {
		return fmt.Errorf("connections.total (%d) must not be below connections.active (%d)", t, a)
	}
	return nil
}

func effectiveConnections(active, total int64) (int, int) {
	if active == 0 {
		active = DefaultConnectionsActive
	}
	if total == 0 {
		total = DefaultConnectionsTotal
	}
	return int(active), int(total)
}

// ConnectionLimits returns the effective connections.active and .total.
func (c *Config) ConnectionLimits() (active, total int) {
	return effectiveConnections(c.ConnectionsActive, c.ConnectionsTotal)
}

// SetConnectionsText sets connections.<key> (active or total) in config text:
// the `connections.<key>:` line is rewritten, or added after the listen and
// connections settings (at the top when there are none). A `connections: {` block must be
// edited by hand.
func SetConnectionsText(text, key string, value int64) (string, error) {
	if key != "active" && key != "total" {
		return "", fmt.Errorf("unknown connections setting %q (active or total)", key)
	}
	lines := strings.Split(text, "\n")
	line := "connections." + key + ": " + strconv.FormatInt(value, 10)
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

var connectionsBlock = regexp.MustCompile(`^connections\s*:`)
