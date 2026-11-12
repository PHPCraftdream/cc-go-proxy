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
	Forked                  // diverged; the source was written under a new id (Options.Fork)
	Replaced                // diverged or ahead; the old destination was kept as a backup and replaced (Options.Force)
	Elsewhere               // the destination has this id in another project folder (Result.Other); left alone
)

func (s Status) String() string {
	return [...]string{"copied", "updated", "unchanged", "ahead", "diverged", "forked", "replaced", "elsewhere"}[s]
}

// Skipped reports statuses where the destination was deliberately not written.
func (s Status) Skipped() bool { return s == Ahead || s == Diverged || s == Elsewhere }

// Options tune a copy.
type Options struct {
	// StripThinking removes thinking blocks, whose signatures are only valid
	// for the provider that produced them.
	StripThinking bool
	// Project, when set, limits the copy to that projects/ folder (the sessions
	// of one working directory); the count then applies within it.
	Project string
	// Fork writes a session whose destination diverged under a new id (the same
	// one every time, so a rerun extends it), leaving the destination alone.
	Fork bool
	// Force replaces a destination that diverged or is ahead of the source; the
	// old file is kept next to it as <name>.bak-<time>. Excludes Fork.
	Force bool
	// rewrite swaps a session id for another in jsonl files (set for forks).
	rewrite *idSwap
	// OnStart, when set, is called before each session; i counts from 1 up to n.
	OnStart func(i, n int, s Session)
	// OnProgress, when set, reports how much of a transcript is processed while
	// it is copied, at most once per ProgressEvery. Short transcripts finish
	// before the first report.
	OnProgress func(s Session, done, total int64)
	// ProgressEvery spaces OnProgress calls; 0 means 2 seconds, a negative value
	// reports after every read.
	ProgressEvery time.Duration
	// OnResult, when set, is called as soon as each session is done, so a
	// caller can report progress on large transcripts.
	OnResult func(Result)
}

const defaultProgressEvery = 2 * time.Second

// Result is the outcome for one session.
type Result struct {
	Session Session
	Status  Status
	Files   int    // sidecar files written
	Removed int    // thinking blocks removed
	Fork    string // id of the fork the session was written as (Options.Fork)
	Backup  string // path the replaced destination was kept at (Options.Force)
	Other   string // project folder of the destination that holds the id (Elsewhere)
	Err     error  // set when the session could not be copied
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
// work done on either side is never lost; other cases are reported as skipped,
// unless Options.Fork or Options.Force says what to do with them.
func Copy(srcDir, dstDir string, count int, opt Options) ([]Result, error) {
	if count < 1 {
		return nil, fmt.Errorf("sessions: count must be at least 1, got %d", count)
	}
	if opt.Fork && opt.Force {
		return nil, errors.New("sessions: fork and force exclude each other")
	}
	if sameDir(srcDir, dstDir) {
		return nil, errors.New("sessions: source and destination are the same directory")
	}
	list, err := List(srcDir)
	if err != nil {
		return nil, err
	}
	if opt.Project != "" {
		list = inProject(list, opt.Project)
	}
	if len(list) > count {
		list = list[:count]
	}
	results := make([]Result, 0, len(list))
	for i, s := range list {
		if opt.OnStart != nil {
			opt.OnStart(i+1, len(list), s)
		}
		res := copySession(dstDir, s, opt)
		if opt.OnResult != nil {
			opt.OnResult(res)
		}
		results = append(results, res)
	}
	return results, nil
}

// inProject keeps the sessions of one project folder, in order.
func inProject(list []Session, project string) []Session {
	var out []Session
	for _, s := range list {
		if strings.EqualFold(s.Project, project) {
			out = append(out, s)
		}
	}
	return out
}

func copySession(dstDir string, s Session, opt Options) Result {
	res := Result{Session: s}
	dstFile := func(id string) string { return filepath.Join(dstDir, projectsDir, s.Project, id+".jsonl") }
	dst := dstFile(s.ID)
	var prog func(done, total int64)
	if opt.OnProgress != nil {
		prog = func(done, total int64) { opt.OnProgress(s, done, total) }
	}
	// A session id is one session for Claude Code: the same id in another
	// project folder is another session, never written over or next to.
	var (
		st      Status
		removed int
		backup  string
		err     error
	)
	if other := idElsewhere(dstDir, s.Project, s.ID); other != "" {
		st, res.Other = Elsewhere, other
	} else {
		st, removed, backup, err = place(s.Path, dst, true, opt, prog)
	}
	if err == nil && (st == Diverged || st == Elsewhere) && opt.Fork {
		id := forkID(s.ID)
		dst = dstFile(id)
		opt.rewrite = &idSwap{old: s.ID, new: id}
		res.Fork = id
		if st, removed, backup, err = place(s.Path, dst, true, opt, prog); st == Copied || st == Updated {
			st = Forked
		}
	}
	if err != nil {
		res.Err = err
		return res
	}
	res.Status, res.Removed, res.Backup = st, removed, backup
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
		st, n, _, err := place(p, filepath.Join(dstSide, rel), strings.HasSuffix(p, ".jsonl"), opt, nil)
		if err != nil {
			return err
		}
		res.Removed += n
		if st == Copied || st == Updated || st == Replaced {
			res.Files++
		}
		return nil
	})
	res.Err = err
	return res
}

// idElsewhere names the project folder of configDir, other than project, that
// holds the transcript of id, when project itself does not; "" otherwise.
func idElsewhere(configDir, project, id string) string {
	root := filepath.Join(configDir, projectsDir)
	if _, err := os.Lstat(filepath.Join(root, project, id+".jsonl")); err == nil {
		return ""
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, d := range dirs {
		if !d.IsDir() || strings.EqualFold(d.Name(), project) {
			continue
		}
		if info, err := os.Lstat(filepath.Join(root, d.Name(), id+".jsonl")); err == nil && info.Mode().IsRegular() {
			return d.Name()
		}
	}
	return ""
}

func sameDir(a, b string) bool {
	ca, err1 := filepath.Abs(a)
	cb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && strings.EqualFold(filepath.Clean(ca), filepath.Clean(cb))
}
