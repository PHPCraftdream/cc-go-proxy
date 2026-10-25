package main

import (
	"encoding/json"
	"io"
)

// writeJSON prints one pretty-printed, newline-terminated JSON document.
func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}
