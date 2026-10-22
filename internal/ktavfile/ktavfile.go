// Package ktavfile reads and writes the files ccgp keeps for itself (logins,
// URL tokens, the mirror manifest) in Ktav, and migrates the JSON files that
// earlier versions wrote.
package ktavfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/securefile"
	ktav "github.com/ktav-lang/golang/pure"
)

const (
	// Ext is the extension of the files this package writes.
	Ext       = ".ktav"
	legacyExt = ".json"
)

// LegacyPath is the JSON file an earlier version kept in place of path.
func LegacyPath(path string) string { return strings.TrimSuffix(path, Ext) + legacyExt }

// Legacy decodes the contents of an old JSON file into v.
type Legacy func(data []byte, v any) error

// Read loads path into v (a pointer). A missing file is fs.ErrNotExist, unless
// the old JSON file is there and legacy is set: it is decoded by legacy,
// rewritten as Ktav and removed.
func Read(path string, v any, legacy Legacy) error {
	b, err := os.ReadFile(path)
	if err == nil {
		if err := ktav.LoadsInto(string(b), v); err != nil {
			return fmt.Errorf("corrupt %s: %w", path, err)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) || legacy == nil {
		return err
	}
	old := LegacyPath(path)
	ob, oerr := os.ReadFile(old)
	if errors.Is(oerr, fs.ErrNotExist) {
		return err
	}
	if oerr != nil {
		return oerr
	}
	if err := legacy(ob, v); err != nil {
		return fmt.Errorf("corrupt %s: %w", old, err)
	}
	if err := Write(path, v); err != nil {
		return fmt.Errorf("migrating %s: %w", old, err)
	}
	return nil
}

// Write stores v (a struct, map or slice, or a pointer to one) in path,
// atomically and readable by the owner only. The old JSON file of the same
// name is removed: its data lives in path now, and a stale copy would only
// keep old secrets around.
func Write(path string, v any) error {
	s, err := Encode(v)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if err := securefile.WriteFile(path, []byte(s)); err != nil {
		return err
	}
	if strings.HasSuffix(path, Ext) {
		_ = os.Remove(LegacyPath(path))
	}
	return nil
}

// Encode renders v as Ktav and checks that the text reads back as the same
// value, so a file that would not load again is never written.
func Encode(v any) (string, error) {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	s, err := ktav.Dumps(rv.Interface())
	if err != nil {
		return "", err
	}
	back := reflect.New(rv.Type())
	if err := ktav.LoadsInto(s, back.Interface()); err != nil {
		return "", fmt.Errorf("does not read back: %w", err)
	}
	if !sameValue(rv.Interface(), back.Elem().Interface()) {
		return "", errors.New("does not read back as the same value")
	}
	return s, nil
}

// sameValue compares by JSON form, where nil and empty collections are alike.
func sameValue(a, b any) bool {
	return reflect.DeepEqual(normalized(a), normalized(b))
}

func normalized(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	var x any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&x)
	return prune(x)
}

func prune(x any) any {
	switch t := x.(type) {
	case map[string]any:
		for k, e := range t {
			if t[k] = prune(e); t[k] == nil {
				delete(t, k)
			}
		}
		if len(t) == 0 {
			return nil
		}
	case []any:
		if len(t) == 0 {
			return nil
		}
		for i, e := range t {
			t[i] = prune(e)
		}
	}
	return x
}
