package angel

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
	"github.com/PHPCraftdream/cc-go-proxy/internal/ktavfile"
)

// A server that ends without a trace leaves no way to tell why: a crash prints
// to the log, but a kill from outside prints nothing. So a running server keeps
// a record of itself next to the log (server.run, its mtime moves every
// heartbeat), and a stop by ccgp or a clean exit removes it. The next server that
// finds the record knows the previous one vanished, and says since when and what
// it ran in. The output of a crash also goes to crash/, one file per crash.
const (
	runFile     = "server.run"
	crashDir    = "crash"
	keepCrashes = 20
	timeFormat  = "2006-01-02 15:04:05"
)

// heartbeat is how often a live server touches its record; the last touch is the
// time it was last alive. Var so tests can shorten it.
var heartbeat = 15 * time.Second

// Run is the record a server keeps about itself. Strings only: it is a Ktav file.
type Run struct {
	PID     string `json:"pid"`
	Started string `json:"started"` // local time
	Version string `json:"version"`
	Parent  string `json:"parent"` // the process that started it
	Job     string `json:"job"`    // what the Windows job around it does, "none" outside of one
}

// Lifecycle is what a running server leaves behind for the next one to read.
type Lifecycle struct {
	Run   Run
	dir   string
	crash *os.File
	stop  chan struct{}
	once  sync.Once
}

func runPath(dir string) string { return filepath.Join(dir, runFile) }

// BeginRun records this server in the logs directory and keeps the output of a
// crash there. note says what became of the previous server when it vanished
// (no stop by ccgp, no clean exit): "" when it ended in order. The Lifecycle is
// nil when the directory cannot be used; its methods allow that.
func BeginRun(version string) (lc *Lifecycle, note string) {
	dir, err := home.LogDir()
	if err != nil {
		return nil, ""
	}
	return beginRun(dir, version)
}

func beginRun(dir, version string) (*Lifecycle, string) {
	if err := os.MkdirAll(filepath.Join(dir, crashDir), 0o700); err != nil {
		return nil, ""
	}
	prev, last, hadPrev := readRun(dir)
	note := vanishedNote(prev, last, hadPrev, reapCrashes(dir))

	if version == "" {
		version = "unknown"
	}
	lc := &Lifecycle{dir: dir, stop: make(chan struct{}), Run: Run{
		PID: strconv.Itoa(os.Getpid()), Started: time.Now().Format(timeFormat), Version: version,
		Parent: strconv.Itoa(os.Getppid()), Job: jobNote(),
	}}
	_ = ktavfile.Write(runPath(dir), lc.Run)
	lc.crash = openCrash(dir)
	go lc.beat(heartbeat)
	return lc, note
}

func (l *Lifecycle) beat(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-t.C:
			_ = os.Chtimes(runPath(l.dir), now, now)
		}
	}
}

// End is a clean exit: the record goes, and so does a crash file that stayed empty.
func (l *Lifecycle) End() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		close(l.stop)
		if l.crash != nil {
			_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
			name := l.crash.Name()
			st, _ := l.crash.Stat()
			_ = l.crash.Close()
			if st == nil || st.Size() == 0 {
				_ = os.Remove(name)
			}
		}
		_ = os.Remove(runPath(l.dir))
	})
}

// readRun reads the record of the previous server and when it was last touched.
func readRun(dir string) (r Run, last time.Time, ok bool) {
	st, err := os.Stat(runPath(dir))
	if err != nil || ktavfile.Read(runPath(dir), &r, nil) != nil {
		return Run{}, time.Time{}, false
	}
	if r.PID == strconv.Itoa(os.Getpid()) {
		return Run{}, time.Time{}, false // our own record: a pid that was reused
	}
	return r, st.ModTime(), true
}

// openCrash makes the file that receives the output of a crash (the panic with
// every goroutine, or the runtime's fatal error) besides the log. It is named
// pending until the next server finds it non-empty (reapCrashes).
func openCrash(dir string) *os.File {
	f, err := os.OpenFile(filepath.Join(dir, crashDir, "pending-"+strconv.Itoa(os.Getpid())+".txt"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil
	}
	debug.SetTraceback("all")
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil
	}
	return f
}

// reapCrashes turns the pending files of servers that are gone into crash
// reports (crash-<time>-pid<N>.txt) when they hold anything, removes the empty
// ones and keeps the newest keepCrashes reports. It returns the names of the
// new reports.
func reapCrashes(dir string) []string {
	cd := filepath.Join(dir, crashDir)
	entries, err := os.ReadDir(cd)
	if err != nil {
		return nil
	}
	var made []string
	for _, e := range entries {
		name := e.Name()
		pid, ok := strings.CutSuffix(strings.TrimPrefix(name, "pending-"), ".txt")
		if !strings.HasPrefix(name, "pending-") || !ok || e.IsDir() {
			continue
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		if st.Size() == 0 {
			_ = os.Remove(filepath.Join(cd, name))
			continue
		}
		report := "crash-" + st.ModTime().Format("20060102-150405") + "-pid" + pid + ".txt"
		_ = os.Remove(filepath.Join(cd, report))
		if os.Rename(filepath.Join(cd, name), filepath.Join(cd, report)) == nil {
			made = append(made, report)
		}
	}
	pruneCrashes(cd)
	sort.Strings(made)
	return made
}

func pruneCrashes(cd string) {
	entries, err := os.ReadDir(cd)
	if err != nil {
		return
	}
	var reports []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "crash-") && strings.HasSuffix(e.Name(), ".txt") {
			reports = append(reports, e.Name())
		}
	}
	sort.Strings(reports) // the time is in the name: oldest first
	for len(reports) > keepCrashes {
		_ = os.Remove(filepath.Join(cd, reports[0]))
		reports = reports[1:]
	}
}

// vanishedNote is the log line about the previous server: it left its record
// (ok) or a crash report, so it was not stopped by ccgp and did not end cleanly.
// "" when there is nothing to report.
func vanishedNote(prev Run, last time.Time, ok bool, crashes []string) string {
	var b strings.Builder
	switch {
	case ok:
		fmt.Fprintf(&b, "previous server pid %s (started %s, last alive %s) ended without a stop by ccgp or a clean exit",
			prev.PID, prev.Started, last.Format(timeFormat))
	case len(crashes) > 0:
		b.WriteString("an earlier server crashed")
	default:
		return ""
	}
	if len(crashes) > 0 {
		fmt.Fprintf(&b, "; crash report: %s (in %s next to the log)", strings.Join(crashes, ", "), crashDir)
	} else {
		b.WriteString("; no crash report, so it was ended from outside (killed by a program or a user, or its job or terminal closed)")
	}
	if ok && prev.Job != "" && prev.Job != "none" {
		b.WriteString("; it ran in a job: " + prev.Job)
	}
	return b.String()
}

// StopBy is Stop that also tells the log who stopped the server and clears its
// record: a missing record reads as a stop in order for the next server.
func StopBy(who string) func(base string, timeout time.Duration) error {
	return func(base string, timeout time.Duration) error {
		h, _ := Check(base)
		if err := Stop(base, timeout); err != nil {
			return err
		}
		if h != nil {
			if dir, err := home.LogDir(); err == nil {
				noteStop(dir, h.PID, who)
			}
		}
		return nil
	}
}

// noteStop appends the stop to the log (which only exists when a server ran)
// and removes the record of the stopped server.
func noteStop(dir string, pid int, who string) {
	line := fmt.Sprintf("[%s] server pid %d stopped by %s (pid %d, started by pid %d)\n",
		time.Now().Format("15:04:05"), pid, who, os.Getpid(), os.Getppid())
	if f, err := os.OpenFile(filepath.Join(dir, "ccgp.log"), os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		_, _ = f.WriteString(line)
		_ = f.Close()
	}
	_ = os.Remove(runPath(dir))
}
