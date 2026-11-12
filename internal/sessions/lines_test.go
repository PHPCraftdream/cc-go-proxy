package sessions

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
)

// drain reads every line of src: the lines joined, the stream bytes counted,
// and the error that ended it.
func drain(t *testing.T, src lineSource) (string, int, error) {
	t.Helper()
	defer src.close()
	var out bytes.Buffer
	total := 0
	for {
		line, n, err := src.next()
		out.Write(line)
		total += n
		if err != nil {
			return out.String(), total, err
		}
	}
}

// randomLines is a stream of lines of every length class, long ones included.
func randomLines(r *rand.Rand, n int) string {
	var b strings.Builder
	for i := range n {
		switch k := r.Intn(100); {
		case k < 10:
			b.WriteString("\n")
		case k == 10:
			b.WriteString(strings.Repeat("L", copyBuf+r.Intn(copyBuf/2)) + "\n")
		case k < 20:
			fmt.Fprintf(&b, "drop %d\n", i)
		default:
			fmt.Fprintf(&b, "line %d %s\n", i, strings.Repeat("x", r.Intn(4000)))
		}
	}
	return b.String()
}

// rewrite changes some lines, drops others and leaves the rest.
func rewrite(b []byte) []byte {
	switch {
	case bytes.HasPrefix(b, []byte("drop")):
		return nil
	case bytes.HasPrefix(b, []byte("line 1")):
		return append([]byte("ONE"), b...)
	}
	return b
}

func TestParallelLinesMatchSequential(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, in := range []string{
		"",
		"no newline at the end",
		"a\nb\n",
		"\n\n\n",
		randomLines(r, 50),
		randomLines(r, 1500) + "tail without newline",
	} {
		want, wantN, wantErr := drain(t, newSeqLines(strings.NewReader(in), rewrite, 0))
		for _, workers := range []int{1, 2, 7} {
			got, n, err := drain(t, newParLines(strings.NewReader(in), rewrite, workers))
			if got != want || n != wantN || n != len(in) || !errors.Is(err, io.EOF) || !errors.Is(wantErr, io.EOF) {
				t.Errorf("%d workers, %d bytes in: got %d bytes (%d read, %v), want %d (%d, %v)",
					workers, len(in), len(got), n, err, len(want), wantN, wantErr)
			}
		}
	}
}

func TestParallelLinesReportTheSourceErrorAfterTheData(t *testing.T) {
	boom := errors.New("boom")
	in := randomLines(rand.New(rand.NewSource(2)), 400)
	got, n, err := drain(t, newParLines(io.MultiReader(strings.NewReader(in), iotest.ErrReader(boom)), rewrite, 4))
	want, _, _ := drain(t, newSeqLines(strings.NewReader(in), rewrite, 0))
	if !errors.Is(err, boom) || got != want || n != len(in) {
		t.Errorf("got %d bytes, %d read, %v; want %d bytes, then boom", len(got), n, err, len(want))
	}
}

func TestParallelLinesStopWhenClosed(t *testing.T) {
	in := randomLines(rand.New(rand.NewSource(3)), 1500)
	p := newParLines(strings.NewReader(in), rewrite, 3)
	if _, _, err := p.next(); err != nil {
		t.Fatal(err)
	}
	p.close()
	p.close()          // twice is fine
	for range 100000 { // the queue empties and ends, nothing blocks
		if _, _, err := p.next(); err != nil {
			return
		}
	}
	t.Error("still yielding lines long after close")
}

func TestReadLineLongLines(t *testing.T) {
	long := strings.Repeat("y", 3*copyBuf+5)
	br := bufio.NewReaderSize(strings.NewReader("a\n"+long+"\nb"), copyBuf)
	var buf []byte
	for _, want := range []string{"a\n", long + "\n", "b"} {
		line, err := readLine(br, &buf)
		if string(line) != want {
			t.Fatalf("got %d bytes, want %d", len(line), len(want))
		}
		if want == "b" && !errors.Is(err, io.EOF) {
			t.Errorf("last line: %v", err)
		}
	}
}

// The ids of a large file found on several goroutines are those found line by line.
func TestForkIDsParallelScanMatchesSequential(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	var b strings.Builder
	for b.Len() < 6*batchSize {
		u := fmt.Sprintf("%08x-0000-4000-8000-%012x", r.Uint32(), r.Int63n(1<<40))
		fmt.Fprintf(&b, `{"uuid":"%s","slug":"calm-%d-river","agentId":"a%016x","text":"with ID: b%07dq done"}`+"\n",
			u, r.Intn(5), r.Uint64(), r.Intn(1e7))
		b.WriteString(strings.Repeat("z", r.Intn(2000)) + "\n")
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	par := &forkIDs{fid: forkID(sid), uuids: map[string]bool{}, short: map[string]bool{}, slugs: map[string]string{}}
	if err := par.scan(path); err != nil {
		t.Fatal(err)
	}
	seq := &forkIDs{fid: forkID(sid), uuids: map[string]bool{}, short: map[string]bool{}, slugs: map[string]string{}}
	br := bufio.NewReader(strings.NewReader(b.String()))
	var long []byte
	for {
		line, err := readLine(br, &long)
		seq.scanLine(line)
		if err != nil {
			break
		}
	}
	if fmt.Sprint(par.uuids) != fmt.Sprint(seq.uuids) || fmt.Sprint(par.short) != fmt.Sprint(seq.short) ||
		fmt.Sprint(par.slugs) != fmt.Sprint(seq.slugs) || len(seq.uuids) < 100 || len(seq.short) < 100 || len(seq.slugs) != 5 {
		t.Errorf("parallel %d/%d/%d ids, sequential %d/%d/%d", len(par.uuids), len(par.short), len(par.slugs),
			len(seq.uuids), len(seq.short), len(seq.slugs))
	}
}

// After prepare, a short id is told by its shape first and mapped once; the
// result is what the map alone says.
func TestPreparedShortIDs(t *testing.T) {
	m := &forkIDs{fid: forkID(sid), short: map[string]bool{}}
	for _, id := range []string{"a0123456789abcdef", "bk7f3m2q1", "bqwertyui", "abc123"} {
		m.add(id)
	}
	plain := &forkIDs{fid: m.fid, short: m.short}
	m.prepare()
	for _, w := range []string{"a0123456789abcdef", "bk7f3m2q1", "bqwertyui", "abc123", "abc124", "a0123456789abcdeg",
		"deployment", "b", "", strings.Repeat("a", 40), "bk7f3m2q"} {
		if m.isShort([]byte(w)) != plain.isShort([]byte(w)) {
			t.Errorf("%q: prepared %v, plain %v", w, m.isShort([]byte(w)), plain.isShort([]byte(w)))
		}
		if plain.short[w] && m.shortFor(w) != plain.mapShort(w) {
			t.Errorf("%q: %s, want %s", w, m.shortFor(w), plain.mapShort(w))
		}
	}
	text := []byte(`{"agentId":"a0123456789abcdef","t":"bk7f3m2q1 and\nbqwertyui, abc123x abc123"}`)
	if got, want := string(m.line(text)), string(plain.line(text)); got != want {
		t.Errorf("line: %s, want %s", got, want)
	}
}

// Many files of a session's folder are copied at once, each to its place.
func TestCopyManySidecarFiles(t *testing.T) {
	p := pair{src: t.TempDir(), dst: t.TempDir()}
	p.s = filepath.Join(p.src, "projects", "a", sid+".jsonl")
	p.d = filepath.Join(p.dst, "projects", "a", sid+".jsonl")
	put(t, p.s, entry(sid, "one"), t0)
	side := strings.TrimSuffix(p.s, ".jsonl")
	for i := range 37 {
		put(t, filepath.Join(side, "tool-results", fmt.Sprintf("t%02d.txt", i)), fmt.Sprintf("result %d of %s", i, sid), t0)
	}
	r := copyOne(t, p, Options{})
	if r.Files != 37 {
		t.Fatalf("files = %d", r.Files)
	}
	for i := range 37 {
		got := read(t, filepath.Join(strings.TrimSuffix(p.d, ".jsonl"), "tool-results", fmt.Sprintf("t%02d.txt", i)))
		if got != fmt.Sprintf("result %d of %s", i, sid) {
			t.Errorf("t%02d: %q", i, got)
		}
	}
	if r = copyOne(t, p, Options{}); r.Status != Unchanged || r.Files != 0 {
		t.Errorf("rerun: %v, %d files", r.Status, r.Files)
	}
}

// A sidecar file that cannot be written fails the session; the others may
// have been written.
func TestCopySidecarFailure(t *testing.T) {
	p := pair{src: t.TempDir(), dst: t.TempDir()}
	p.s = filepath.Join(p.src, "projects", "a", sid+".jsonl")
	put(t, p.s, entry(sid, "one"), t0)
	side := strings.TrimSuffix(p.s, ".jsonl")
	for i := range 9 {
		put(t, filepath.Join(side, "tool-results", fmt.Sprintf("t%d.txt", i)), "x", t0)
	}
	// A directory where a file must go.
	if err := os.MkdirAll(filepath.Join(p.dst, "projects", "a", sid, "tool-results", "t4.txt", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := Copy(p.src, p.dst, 1, Options{})
	if err != nil || len(res) != 1 || res[0].Err == nil {
		t.Fatalf("Copy = %+v, %v; want the session failed", res, err)
	}
}

// afterClose fails the test when the stream is read once closed is set.
type afterClose struct {
	r      io.Reader
	closed *atomic.Bool
	late   *atomic.Bool
}

func (a afterClose) Read(p []byte) (int, error) {
	if a.closed.Load() {
		a.late.Store(true)
	}
	return a.r.Read(p)
}

// Nothing reads the stream (or reports its progress) once close has returned.
func TestParallelLinesCloseWaitsForTheReader(t *testing.T) {
	in := randomLines(rand.New(rand.NewSource(5)), 1500)
	for range 20 {
		var closed, late atomic.Bool
		p := newParLines(afterClose{strings.NewReader(in), &closed, &late}, rewrite, 3)
		if _, _, err := p.next(); err != nil {
			t.Fatal(err)
		}
		p.close()
		closed.Store(true)
		runtime.Gosched()
		if late.Load() {
			t.Fatal("the stream was read after close")
		}
	}
}

// A small stream is not worth goroutines; a large one with a rewrite is.
func TestNewLinesChoosesBySize(t *testing.T) {
	if _, ok := newLines(strings.NewReader(""), rewrite, parallelFrom-1).(*seqLines); !ok {
		t.Error("a small stream went parallel")
	}
	if _, ok := newLines(strings.NewReader(""), nil, parallelFrom).(*seqLines); !ok {
		t.Error("a stream without a rewrite went parallel")
	}
	if runtime.GOMAXPROCS(0) > 1 {
		src := newLines(strings.NewReader(""), rewrite, parallelFrom)
		if _, ok := src.(*parLines); !ok {
			t.Error("a large stream was not parallel")
		}
		src.close()
	}
}
