// Package home decides where ccgp keeps its own files: the config, the secrets
// (creds), the isolated Claude Code dirs and the server log.
//
// By default they live in the user's config and cache dirs. A home directory
// (--home <dir>, --local, CCGP_HOME or a .ccgp directory in the current
// directory) moves all of them into one place, so a
// second setup shares nothing with the first: settings, logins, URL tokens,
// Claude Code history and the log.
package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// EnvVar names the home directory. The flags set it for the process, so the
	// background server and everything else ccgp starts use the same home.
	EnvVar = "CCGP_HOME"
	// LocalDir is the directory --local keeps in the current directory.
	LocalDir = ".ccgp"

	appDir = "cc-go-proxy"
)

// Root is the home directory in use, or "" for the default layout.
func Root() string { return os.Getenv(EnvVar) }

// Dir is where config.ktav, creds/ and claude/ live.
func Dir() (string, error) {
	if r := Root(); r != "" {
		return r, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, appDir), nil
}

// LogDir is where ccgp.log lives.
func LogDir() (string, error) {
	if r := Root(); r != "" {
		return filepath.Join(r, "logs"), nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, appDir, "logs"), nil
}

// Set makes dir the home of this process and of every process it starts.
// A relative dir is taken from the current directory. The directory itself is
// not created (see Create).
func Set(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return abs, os.Setenv(EnvVar, abs)
}

// Create makes the home directory (owner-only). A directory that is new gets a
// .gitignore holding "*": it keeps logins and URL tokens, and --local puts it
// into a project that is usually a repository.
func Create(dir string) error {
	_, err := os.Stat(dir)
	fresh := errors.Is(err, os.ErrNotExist)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if !fresh {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o600)
}

// Options are the home flags at the start of a command line.
type Options struct {
	Local bool   // --local: <current directory>/.ccgp
	Dir   string // --home <dir>
}

// Parse takes the home flags off the start of args: --local, --home <dir> and
// --home=<dir>. They come before the profile or the command, so they never
// clash with the arguments that go on to claude.
func Parse(args []string) (Options, []string, error) {
	var o Options
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "--local":
			if o.Local {
				return o, nil, errors.New("--local given twice")
			}
			o.Local, args = true, args[1:]
		case a == "--home" || strings.HasPrefix(a, "--home="):
			if o.Dir != "" {
				return o, nil, errors.New("--home given twice")
			}
			val, rest := strings.TrimPrefix(a, "--home="), args[1:]
			if a == "--home" {
				if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
					return o, nil, errors.New("--home needs a directory (ccgp --home <dir>)")
				}
				val, rest = rest[0], rest[1:]
			}
			if val == "" {
				return o, nil, errors.New("--home needs a directory (ccgp --home <dir>)")
			}
			o.Dir, args = val, rest
		default:
			if o.Local && o.Dir != "" {
				return o, nil, errors.New("use --local or --home <dir>, not both")
			}
			return o, args, nil
		}
	}
	if o.Local && o.Dir != "" {
		return o, nil, errors.New("use --local or --home <dir>, not both")
	}
	return o, args, nil
}

// Any reports whether a flag was given.
func (o Options) Any() bool { return o.Local || o.Dir != "" }

// Args is the flags as they were typed, to hand them on (ccgpy passes them to ccgp).
func (o Options) Args() []string {
	switch {
	case o.Local:
		return []string{"--local"}
	case o.Dir != "":
		return []string{"--home", o.Dir}
	}
	return nil
}

// Path is the directory the flags name, absolute; "" when no flag was given.
func (o Options) Path() (string, error) {
	switch {
	case o.Local:
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(wd, LocalDir), nil
	case o.Dir != "":
		return filepath.Abs(o.Dir)
	}
	return "", nil
}

// Detect finds a .ccgp directory in the current directory (only there,
// not in the parents): a project that has one runs on it without any flag.
func Detect() (string, bool) {
	wd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	dir := filepath.Join(wd, LocalDir)
	st, err := os.Stat(dir)
	return dir, err == nil && st.IsDir()
}

// Use makes the home take effect for this process and its children. The order:
// a flag, then CCGP_HOME, then a .ccgp directory in the current
// directory, then the default layout. With create the flag's directory is made
// as well; without it (completion) nothing is written.
func (o Options) Use(create bool) error {
	dir, err := o.Path()
	if err != nil {
		return err
	}
	if dir == "" {
		if Root() != "" {
			return nil
		}
		if d, ok := Detect(); ok {
			_, err = Set(d)
			return err
		}
		return nil
	}
	if create {
		if err := Create(dir); err != nil {
			return fmt.Errorf("home %s: %w", dir, err)
		}
	}
	_, err = Set(dir)
	return err
}
