package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

const connectionsUsage = "usage: ccgp --connections [active N] [total N] [--json]"

func connectionsCmd(args []string) error { return connections(os.Stdout, args) }

// connections shows connections.active / .total, or sets them in config.ktav
// (the running server applies them on its next request).
func connections(w io.Writer, args []string) error {
	jsonOut := false
	set := map[string]int64{}
	var order []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--json":
			jsonOut = true
		case "active", "total":
			if i+1 >= len(args) {
				return errors.New(connectionsUsage)
			}
			n, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil || n < 0 {
				return fmt.Errorf("%s: want a number (0: default), got %q", a, args[i+1])
			}
			if _, dup := set[a]; !dup {
				order = append(order, a)
			}
			set[a] = n
			i++
		default:
			return errors.New(connectionsUsage)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	backup := ""
	if len(set) > 0 {
		if cfg.IsEnvMode() {
			return errors.New("no config.ktav (env-only mode): nothing to edit")
		}
		backup, err = config.EditFile(cfg.ConfigFile, func(text string) (string, error) {
			for _, k := range order {
				var err error
				if text, err = config.SetConnectionsText(text, k, set[k]); err != nil {
					return "", err
				}
			}
			return text, nil
		})
		if err != nil {
			return err
		}
		if cfg, err = config.Load(); err != nil {
			return err
		}
	}
	active, total := cfg.ConnectionLimits()
	if jsonOut {
		return writeJSON(w, map[string]any{
			"active": active, "total": total,
			"active_default": cfg.ConnectionsActive == 0, "total_default": cfg.ConnectionsTotal == 0,
		})
	}
	def := func(v int64) string {
		if v == 0 {
			return " (default)"
		}
		return ""
	}
	_, _ = fmt.Fprintf(w, "active: %-3d connections per backend that take new requests%s\n", active, def(cfg.ConnectionsActive))
	_, _ = fmt.Fprintf(w, "total:  %-3d active plus ones still finishing older requests%s\n", total, def(cfg.ConnectionsTotal))
	if len(set) > 0 {
		if backup != "" {
			_, _ = fmt.Fprintf(w, "config.ktav updated (old one saved as %s); the running server applies it on its next request\n", backup)
		} else {
			_, _ = fmt.Fprintln(w, "config.ktav already had these values")
		}
	}
	return nil
}
