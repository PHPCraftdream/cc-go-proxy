// Package sessions copies Claude Code session transcripts between two config
// directories: the user's own one and a ccgp profile's isolated one.
//
// A session is projects/<project>/<id>.jsonl plus the optional sibling
// directory projects/<project>/<id>/ (tool results, subagent transcripts).
// Nothing else is touched, in particular no credentials, prompt history or
// live-process registry.
package sessions

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const projectsDir = "projects"

// Session is one transcript found under <config dir>/projects.
type Session struct {
	Project string // directory name, derived by Claude Code from the working directory
	ID      string
	Path    string
	Size    int64
	ModTime time.Time
}

// Status says what happened to a session's transcript.
type Status int

const (
	Copied    Status = iota // not present at the destination before
	Updated                 // destination was an earlier part of the source and got extended
	Unchanged               // already identical
	Ahead                   // destination holds more than the source; left alone
	Diverged                // both sides changed; destination left alone
)

func (s Status) String() string {
	return [...]string{"copied", "updated", "unchanged", "ahead", "diverged"}[s]
}

// Skipped reports statuses where the destination was deliberately not written.
func (s Status) Skipped() bool { return s == Ahead || s == Diverged }

// Options tune a copy.
type Options struct {
	// StripThinking removes thinking blocks, whose signatures are only valid
	// for the provider that produced them.
	StripThinking bool
	// OnResult, when set, is called as soon as each session is done, so a
	// caller can report progress on large transcripts.
	OnResult func(Result)
}

// Result is the outcome for one session.
type Result struct {
	Session Session
	Status  Status
	Files   int   // sidecar files written
	Removed int   // thinking blocks removed
	Err     error // set when the session could not be copied
}

// List returns the sessions of a config dir, most recently modified first.
func List(configDir string) ([]Session, error) {
	root := filepath.Join(configDir, projectsDir)
	projects, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, p := range projects {
		if !p.IsDir() { // links are not followed
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, p.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if !f.Type().IsRegular() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue // vanished meanwhile
			}
			out = append(out, Session{
				Project: p.Name(),
				ID:      strings.TrimSuffix(f.Name(), ".jsonl"),
				Path:    filepath.Join(root, p.Name(), f.Name()),
				Size:    info.Size(),
				ModTime: info.ModTime(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ModTime.Equal(out[j].ModTime) {
			return out[i].ModTime.After(out[j].ModTime)
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// Copy copies the count most recently modified sessions of srcDir into dstDir.
// The source is only read. An existing destination file is written only when
// it is an earlier part of the new content (transcripts are append-only), so
// work done on either side is never lost; other cases are reported as skipped.
func Copy(srcDir, dstDir string, count int, opt Options) ([]Result, error) {
	if count < 1 {
		return nil, fmt.Errorf("sessions: count must be at least 1, got %d", count)
	}
	if sameDir(srcDir, dstDir) {
		return nil, errors.New("sessions: source and destination are the same directory")
	}
	list, err := List(srcDir)
	if err != nil {
		return nil, err
	}
	if len(list) > count {
		list = list[:count]
	}
	results := make([]Result, 0, len(list))
	for _, s := range list {
		res := copySession(dstDir, s, opt)
		if opt.OnResult != nil {
			opt.OnResult(res)
		}
		results = append(results, res)
	}
	return results, nil
}

func copySession(dstDir string, s Session, opt Options) Result {
	res := Result{Session: s}
	dst := filepath.Join(dstDir, projectsDir, s.Project, s.ID+".jsonl")
	st, removed, err := syncFile(s.Path, dst, true, opt)
	if err != nil {
		res.Err = err
		return res
	}
	res.Status, res.Removed = st, removed
	if st.Skipped() {
		return res
	}

	// Sidecar files (tool results, subagent transcripts) belong to the transcript.
	srcSide := strings.TrimSuffix(s.Path, ".jsonl")
	if info, err := os.Lstat(srcSide); err != nil || !info.IsDir() {
		return res
	}
	dstSide := strings.TrimSuffix(dst, ".jsonl")
	err = filepath.WalkDir(srcSide, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(srcSide, p)
		if err != nil {
			return err
		}
		st, n, err := syncFile(p, filepath.Join(dstSide, rel), strings.HasSuffix(p, ".jsonl"), opt)
		if err != nil {
			return err
		}
		res.Removed += n
		if st == Copied || st == Updated {
			res.Files++
		}
		return nil
	})
	res.Err = err
	return res
}

func sameDir(a, b string) bool {
	ca, err1 := filepath.Abs(a)
	cb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && strings.EqualFold(filepath.Clean(ca), filepath.Clean(cb))
}
