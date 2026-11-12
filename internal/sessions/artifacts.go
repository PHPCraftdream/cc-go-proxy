package sessions

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// What Claude Code keeps of a session outside its transcript and the folder
// next to it. A copy without them shows no tasks, cannot /rewind and has lost
// its plan.
//
//	tasks/<session id>/<n>.json, .highwatermark   the task list
//	file-history/<session id>/<hash>@v<n>         file versions for /rewind
//	plans/<slug>.md                               the plan, named by the slug in the transcript
//
// session-env/<session id>/ is not copied: hooks write it anew when a session
// starts or resumes, and a profile's holds ccgp's own environment.
const (
	tasksDir   = "tasks"
	historyDir = "file-history"
	plansDir   = "plans"
)

// syncArtifacts brings the artifacts of session s (in srcDir) to the session
// `as` in dstDir. It is called only for a transcript the destination did not
// go its own way on, so what the destination holds is at best an earlier copy.
// ids is set for a fork.
func syncArtifacts(srcDir, dstDir string, s Session, as string, opt Options, res *Result) error {
	var err error
	if res.Tasks, err = syncTasks(srcDir, dstDir, s.ID, as, opt.rewrite); err != nil {
		return err
	}
	if res.History, err = syncHistory(srcDir, dstDir, s.ID, as, opt); err != nil {
		return err
	}
	res.Plans, err = syncPlans(srcDir, dstDir, s.Path, opt.rewrite)
	return err
}

// syncTasks makes the task list of session `as` in dstDir the one session id
// has in srcDir:
//
//   - a task file that differs is written anew, a task the source no longer
//     has is taken out of the list;
//   - nothing newer is lost: a destination file changed after the source's is
//     kept as <name>.bak-<time> (Claude Code reads *.json only), and so is a
//     task taken out;
//   - ids, when set (a fork), gives the ids in the task texts their fork ids;
//     task numbers stay, they are the list's own.
//
// It returns how many files it wrote or took out; a session without tasks at
// the source leaves the destination alone.
func syncTasks(srcDir, dstDir, id, as string, ids *forkIDs) (int, error) {
	src := filepath.Join(srcDir, tasksDir, id)
	entries, err := os.ReadDir(src)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	dst := filepath.Join(dstDir, tasksDir, as)
	stamp := time.Now().Format("20060102-150405")
	have := map[string]bool{}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !taskFile(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // vanished meanwhile
		}
		data, err := os.ReadFile(filepath.Join(src, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return n, err
		}
		have[name] = true
		if ids != nil && strings.HasSuffix(name, ".json") {
			data = ids.line(data)
		}
		wrote, err := putFile(filepath.Join(dst, name), data, info.ModTime(), stamp)
		if err != nil {
			return n, err
		}
		if wrote {
			n++
		}
	}
	if len(have) == 0 {
		return 0, nil
	}
	// Tasks the source deleted since the last copy.
	old, err := os.ReadDir(dst)
	if err != nil {
		return n, nil
	}
	for _, e := range old {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".json") || have[name] {
			continue
		}
		if err := os.Rename(filepath.Join(dst, name), backupName(filepath.Join(dst, name), stamp)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// taskFile tells the files of a task list that are copied: the tasks and the
// high-water mark. .lock is each Claude Code's own.
func taskFile(name string) bool {
	return strings.HasSuffix(name, ".json") || name == ".highwatermark"
}

// syncHistory copies the file versions of session id into those of `as`. A
// version is written once and never changes, so the files go by the rule of
// the transcript: one the destination holds differently is left alone, or
// replaced with a backup under Options.Force. They are files of the user's
// project, copied byte for byte (no ids are looked for in them). It returns
// how many files it wrote.
func syncHistory(srcDir, dstDir, id, as string, opt Options) (int, error) {
	src := filepath.Join(srcDir, historyDir, id)
	dst := filepath.Join(dstDir, historyDir, as)
	plain := Options{Force: opt.Force}
	n := 0
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == src {
				return filepath.SkipDir
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if sameFile(d, to) {
			return nil
		}
		st, _, _, err := place(p, to, false, plain, nil)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // vanished meanwhile
		}
		if err != nil {
			return err
		}
		if st == Copied || st == Updated || st == Replaced {
			n++
		}
		return nil
	})
	return n, err
}

// sameFile says the file at path has the size and the time of the source
// entry: a copy place made earlier (it sets the source's time), not read again.
func sameFile(src fs.DirEntry, path string) bool {
	a, err := src.Info()
	if err != nil {
		return false
	}
	b, err := os.Stat(path)
	return err == nil && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// syncPlans copies the plan files of the session: plans/<slug>.md for each
// slug its transcript names. A plan that differs is written anew; one changed
// at the destination after the source's is kept as a backup first. A fork has
// slugs of its own (ids.slugs), so that it and the original, in one Claude
// Code, do not write the same plan file. It returns how many files it wrote.
func syncPlans(srcDir, dstDir, transcript string, ids *forkIDs) (int, error) {
	slugs := map[string]string{}
	if ids != nil {
		slugs = ids.slugs
	} else {
		for _, s := range transcriptSlugs(transcript) {
			slugs[s] = s
		}
	}
	stamp := time.Now().Format("20060102-150405")
	n := 0
	for old, as := range slugs {
		src := filepath.Join(srcDir, plansDir, old+".md")
		info, err := os.Stat(src)
		if err != nil || !info.Mode().IsRegular() {
			continue // a session that never planned
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return n, err
		}
		if ids != nil {
			data = ids.line(data)
		}
		wrote, err := putFile(filepath.Join(dstDir, plansDir, as+".md"), data, info.ModTime(), stamp)
		if err != nil {
			return n, err
		}
		if wrote {
			n++
		}
	}
	return n, nil
}

// slugField is the slug a transcript line names: three words Claude Code
// makes up for the session, the name of its plan file.
var slugField = regexp.MustCompile(`"slug"\s*:\s*"([A-Za-z0-9][A-Za-z0-9_-]{1,80})"`)

// transcriptSlugs is the slugs named at the start and the end of a transcript
// (a session keeps its slug; the whole file is not read for it).
func transcriptSlugs(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	const window = 4 << 20
	seen := map[string]bool{}
	var out []string
	take := func(b []byte) {
		for _, m := range slugField.FindAllSubmatch(b, -1) {
			if s := string(m[1]); !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	head, _ := io.ReadAll(io.LimitReader(bufio.NewReader(f), window))
	take(head)
	if info, err := f.Stat(); err == nil && info.Size() > window {
		tail := make([]byte, min(window, info.Size()-window))
		if n, _ := f.ReadAt(tail, info.Size()-int64(len(tail))); n > 0 {
			take(tail[:n])
		}
	}
	return out
}

// putFile writes data to path unless it is there already. A file there that
// was changed after mtime (the source's) is kept as a backup first.
func putFile(path string, data []byte, mtime time.Time, stamp string) (bool, error) {
	old, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(old, data):
		return false, nil
	case err == nil:
		if info, serr := os.Stat(path); serr == nil && info.ModTime().After(mtime) {
			if err := os.Rename(path, backupName(path, stamp)); err != nil {
				return false, err
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	tmp := path + ".ccgp-tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return false, err
	}
	if err := os.Chtimes(tmp, mtime, mtime); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// backupName is path.bak-<stamp>, numbered when taken.
func backupName(path, stamp string) string {
	name := path + ".bak-" + stamp
	for i := 1; ; i++ {
		if _, err := os.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			return name
		}
		name = fmt.Sprintf("%s.bak-%s-%d", path, stamp, i)
	}
}
