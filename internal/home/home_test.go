package home

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultLayoutWithoutAHome(t *testing.T) {
	t.Setenv(EnvVar, "")
	cfg, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	logs, err := LogDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cfg) != "cc-go-proxy" || filepath.Base(filepath.Dir(logs)) != "cc-go-proxy" || filepath.Base(logs) != "logs" {
		t.Errorf("config dir %q, log dir %q", cfg, logs)
	}
	if Root() != "" {
		t.Errorf("Root = %q", Root())
	}
}

func TestHomeHoldsEverything(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvVar, dir)
	cfg, _ := Dir()
	logs, _ := LogDir()
	if cfg != dir || logs != filepath.Join(dir, "logs") {
		t.Errorf("config dir %q, log dir %q", cfg, logs)
	}
}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []string
		want Options
		rest []string
	}{
		{"none", []string{"codex", "-p", "x"}, Options{}, []string{"codex", "-p", "x"}},
		{"empty", nil, Options{}, nil},
		{"local profile", []string{"--local", "codex", "--local"}, Options{Local: true}, []string{"codex", "--local"}},
		{"home separate", []string{"--home", "d", "--status"}, Options{Dir: "d"}, []string{"--status"}},
		{"home equals", []string{"--home=d e", "codex"}, Options{Dir: "d e"}, []string{"codex"}},
		{"flags only", []string{"--local"}, Options{Local: true}, nil},
		{"after the command it is claude's", []string{"codex", "--home", "x"}, Options{}, []string{"codex", "--home", "x"}},
	} {
		got, rest, err := Parse(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want || !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("%s: got %+v %q, want %+v %q", c.name, got, rest, c.want, c.rest)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for in, want := range map[string]string{
		"--home":                 "needs a directory",
		"--home --status":        "needs a directory",
		"--home=":                "needs a directory",
		"--local --local codex":  "twice",
		"--home a --home=b x":    "twice",
		"--local --home d codex": "not both",
		"--home d --local":       "not both",
	} {
		_, _, err := Parse(strings.Fields(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) error = %v, want %q", in, err, want)
		}
	}
}

func TestArgsAreWhatWasTyped(t *testing.T) {
	for _, o := range []Options{{}, {Local: true}, {Dir: "some dir"}} {
		back, rest, err := Parse(append(o.Args(), "codex"))
		if err != nil || back != o || !reflect.DeepEqual(rest, []string{"codex"}) {
			t.Errorf("%+v -> %q -> %+v %q %v", o, o.Args(), back, rest, err)
		}
	}
	if (Options{}).Any() || !(Options{Local: true}).Any() || !(Options{Dir: "x"}).Any() {
		t.Error("Any")
	}
}

func TestLocalIsInTheCurrentDirectory(t *testing.T) {
	wd := t.TempDir()
	t.Chdir(wd)
	got, err := Options{Local: true}.Path()
	if err != nil {
		t.Fatal(err)
	}
	// The temp dir may be a symlink (macOS): compare what the directory really is.
	want, _ := filepath.EvalSymlinks(wd)
	gotDir, _ := filepath.EvalSymlinks(filepath.Dir(got))
	if filepath.Base(got) != LocalDir || gotDir != want {
		t.Errorf("Path = %q, want %q in %q", got, LocalDir, want)
	}
}

func TestRelativeHomeBecomesAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	p, err := Options{Dir: filepath.Join("a", "b")}.Path()
	if err != nil || !filepath.IsAbs(p) || !strings.HasSuffix(p, filepath.Join("a", "b")) {
		t.Errorf("Path = %q, %v", p, err)
	}
	if p, err := (Options{}).Path(); p != "" || err != nil {
		t.Errorf("no flag: %q, %v", p, err)
	}
}

// Use sets CCGP_HOME for the process (and so for the server it starts), creates
// the directory when asked and keeps a git repository from picking it up.
func TestUseCreatesAndSets(t *testing.T) {
	t.Setenv(EnvVar, "")
	dir := filepath.Join(t.TempDir(), "x", "home")
	if err := (Options{Dir: dir}).Use(false); err != nil {
		t.Fatal(err)
	}
	if Root() != dir {
		t.Errorf("Root = %q, want %q", Root(), dir)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("Use(false) created the directory")
	}
	if err := (Options{Dir: dir}).Use(true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || string(b) != "*\n" {
		t.Errorf(".gitignore = %q, %v", b, err)
	}
}

func TestCreateKeepsAnExistingDirectoryAsItIs(t *testing.T) {
	dir := t.TempDir()
	if err := Create(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); err == nil {
		t.Error("a .gitignore was added to a directory that already existed")
	}
}

func TestNoFlagKeepsTheVariable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvVar, dir)
	if err := (Options{}).Use(true); err != nil || Root() != dir {
		t.Errorf("Root = %q, %v", Root(), err)
	}
	other := t.TempDir()
	if err := (Options{Dir: other}).Use(false); err != nil || Root() != other {
		t.Errorf("a flag must override the variable: Root = %q, %v", Root(), err)
	}
}

func TestAProjectDirectoryIsPickedUp(t *testing.T) {
	t.Setenv(EnvVar, "")
	wd := t.TempDir()
	t.Chdir(wd)
	if _, ok := Detect(); ok {
		t.Fatal("found a home in a directory without one")
	}
	if err := (Options{}).Use(true); err != nil || Root() != "" {
		t.Fatalf("no home: Root = %q, %v", Root(), err)
	}
	// a file of that name is not a home
	if err := os.WriteFile(filepath.Join(wd, LocalDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Detect(); ok {
		t.Fatal("a file was taken for a home")
	}
	if err := os.Remove(filepath.Join(wd, LocalDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(wd, LocalDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (Options{}).Use(true); err != nil {
		t.Fatal(err)
	}
	got, _ := filepath.EvalSymlinks(Root())
	want, _ := filepath.EvalSymlinks(filepath.Join(wd, LocalDir))
	if got == "" || got != want {
		t.Errorf("Root = %q, want %q", got, want)
	}
}

func TestParentsAreNotSearched(t *testing.T) {
	t.Setenv(EnvVar, "")
	wd := t.TempDir()
	if err := os.Mkdir(filepath.Join(wd, LocalDir), 0o700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(wd, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if _, ok := Detect(); ok {
		t.Error("a home of a parent directory was picked up")
	}
}

func TestOrderFlagVariableDirectory(t *testing.T) {
	wd := t.TempDir()
	if err := os.Mkdir(filepath.Join(wd, LocalDir), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wd)
	env := t.TempDir()
	t.Setenv(EnvVar, env)
	if err := (Options{}).Use(true); err != nil || Root() != env {
		t.Errorf("the variable must win over the directory: Root = %q, %v", Root(), err)
	}
	flag := t.TempDir()
	if err := (Options{Dir: flag}).Use(true); err != nil || Root() != flag {
		t.Errorf("the flag must win: Root = %q, %v", Root(), err)
	}
}
