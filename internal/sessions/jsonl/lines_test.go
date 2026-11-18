package jsonl

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
)

// drain reads every line of src: the lines joined, the stream bytes counted,
// and the error that ended it.
func drain(t *testing.T, src Source) (string, int, error) {
	t.Helper()
	defer src.Close()
	var out bytes.Buffer
	total := 0
	for {
		line, _, n, err := src.Next()
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
			b.WriteString(strings.Repeat("L", CopyBuf+r.Intn(CopyBuf/2)) + "\n")
		case k < 20:
			fmt.Fprintf(&b, "drop %d\n", i)
		default:
			fmt.Fprintf(&b, "line %d %s\n", i, strings.Repeat("x", r.Intn(4000)))
		}
	}
	return b.String()
}

// rewriteFn is rewrite as a LineFn.
var rewriteFn = plainLineFn(rewrite)

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
		want, wantN, wantErr := drain(t, NewSeqLines(strings.NewReader(in), rewriteFn, 0))
		for _, workers := range []int{1, 2, 7} {
			got, n, err := drain(t, newParLines(strings.NewReader(in), rewriteFn, workers))
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
	got, n, err := drain(t, newParLines(io.MultiReader(strings.NewReader(in), iotest.ErrReader(boom)), rewriteFn, 4))
	want, _, _ := drain(t, NewSeqLines(strings.NewReader(in), rewriteFn, 0))
	if !errors.Is(err, boom) || got != want || n != len(in) {
		t.Errorf("got %d bytes, %d read, %v; want %d bytes, then boom", len(got), n, err, len(want))
	}
}

func TestParallelLinesStopWhenClosed(t *testing.T) {
	in := randomLines(rand.New(rand.NewSource(3)), 1500)
	p := newParLines(strings.NewReader(in), rewriteFn, 3)
	if _, _, _, err := p.Next(); err != nil {
		t.Fatal(err)
	}
	p.Close()
	p.Close()          // twice is fine
	for range 100000 { // the queue empties and ends, nothing blocks
		if _, _, _, err := p.Next(); err != nil {
			return
		}
	}
	t.Error("still yielding lines long after close")
}

func TestReadLineLongLines(t *testing.T) {
	long := strings.Repeat("y", 3*CopyBuf+5)
	br := bufio.NewReaderSize(strings.NewReader("a\n"+long+"\nb"), CopyBuf)
	var buf []byte
	for _, want := range []string{"a\n", long + "\n", "b"} {
		line, err := ReadLine(br, &buf)
		if string(line) != want {
			t.Fatalf("got %d bytes, want %d", len(line), len(want))
		}
		if want == "b" && !errors.Is(err, io.EOF) {
			t.Errorf("last line: %v", err)
		}
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
		p := newParLines(afterClose{strings.NewReader(in), &closed, &late}, rewriteFn, 3)
		if _, _, _, err := p.Next(); err != nil {
			t.Fatal(err)
		}
		p.Close()
		closed.Store(true)
		runtime.Gosched()
		if late.Load() {
			t.Fatal("the stream was read after close")
		}
	}
}

// A small stream is not worth goroutines; a large one with a rewrite is.
func TestNewLinesChoosesBySize(t *testing.T) {
	if _, ok := NewLines(strings.NewReader(""), rewriteFn, ParallelFrom-1).(*seqLines); !ok {
		t.Error("a small stream went parallel")
	}
	if _, ok := NewLines(strings.NewReader(""), nil, ParallelFrom).(*seqLines); !ok {
		t.Error("a stream without a rewrite went parallel")
	}
	if runtime.GOMAXPROCS(0) > 1 {
		src := NewLines(strings.NewReader(""), rewriteFn, ParallelFrom)
		if _, ok := src.(*parLines); !ok {
			t.Error("a large stream was not parallel")
		}
		src.Close()
	}
}

// plainLineFn is a LineFn of fn, without notes.
func plainLineFn(fn func([]byte) []byte) LineFn {
	if fn == nil {
		return nil
	}
	return func(b []byte) ([]byte, any) { return fn(b), nil }
}
