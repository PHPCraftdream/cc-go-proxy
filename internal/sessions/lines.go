package sessions

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"runtime"
	"sync"
)

// batchSize is about how much of a stream one worker takes at a time.
const batchSize = 1 << 20

// workers is how many lines are worked on at once.
func workers() int { return min(runtime.GOMAXPROCS(0), 8) }

// lineFn is the work on one line that needs no state: it may run on any
// goroutine. It returns the line's new form and a note for the work that
// follows in order (nil: none).
type lineFn func(line []byte) ([]byte, any)

// plainLineFn is a lineFn of fn, without notes.
func plainLineFn(fn func([]byte) []byte) lineFn {
	if fn == nil {
		return nil
	}
	return func(b []byte) ([]byte, any) { return fn(b), nil }
}

// lineSource yields the lines of a stream, each passed through a lineFn. A
// line is valid until the next call. n is the line's length in the stream; out
// is empty when the lineFn drops it. err comes with the last line or after it.
type lineSource interface {
	next() (out []byte, note any, n int, err error)
	close()
}

// readLine reads up to and including the next '\n', or to the end, without a
// copy for lines that fit br's buffer; a longer one is put together in *long.
func readLine(br *bufio.Reader, long *[]byte) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		return line, err
	}
	*long = append((*long)[:0], line...)
	for errors.Is(err, bufio.ErrBufferFull) {
		line, err = br.ReadSlice('\n')
		*long = append(*long, line...)
	}
	return *long, err
}

// seqLines is a lineSource working line after line.
type seqLines struct {
	br   *bufio.Reader
	fn   lineFn // nil: lines as they are
	long []byte
}

// newSeqLines reads r, which holds about size bytes (0: unknown).
func newSeqLines(r io.Reader, fn lineFn, size int64) *seqLines {
	buf := copyBuf
	if size > 0 && size < copyBuf {
		buf = max(int(size), 4<<10)
	}
	return &seqLines{br: bufio.NewReaderSize(r, buf), fn: fn}
}

func (s *seqLines) next() ([]byte, any, int, error) {
	line, err := readLine(s.br, &s.long)
	n := len(line)
	var note any
	if n > 0 && s.fn != nil {
		line, note = s.fn(line)
	}
	return line, note, n, err
}

func (s *seqLines) close() {}

// lineBatch is a run of whole lines of a stream and what fn made of them.
type lineBatch struct {
	data  []byte
	err   error    // of the stream, after data
	ends  []int    // end of each line in data
	out   [][]byte // each line rewritten
	notes []any
	done  chan struct{}
}

func (b *lineBatch) run(fn lineFn) {
	for start := 0; start < len(b.data); {
		end := len(b.data)
		if i := bytes.IndexByte(b.data[start:], '\n'); i >= 0 {
			end = start + i + 1
		}
		out, note := fn(b.data[start:end])
		b.ends = append(b.ends, end)
		b.out = append(b.out, out)
		b.notes = append(b.notes, note)
		start = end
	}
}

// batchBufs recycles the buffers of batches: a large file has thousands.
var batchBufs = sync.Pool{New: func() any { return make([]byte, 0, batchSize+copyBuf) }}

// free gives the batch's buffer back; its lines must not be used after.
func (b *lineBatch) free() {
	if cap(b.data) <= 2*(batchSize+copyBuf) { // one grown by a huge line is not kept
		batchBufs.Put(b.data[:0]) //nolint:staticcheck // a slice header is small
	}
	b.data, b.out, b.notes = nil, nil, nil
}

// readBatches cuts r into runs of whole lines of about batchSize bytes and
// hands them to emit, in order; the last one carries the stream's error
// (io.EOF at its end). It stops when emit returns false.
func readBatches(r io.Reader, emit func(*lineBatch) bool) {
	br := bufio.NewReaderSize(r, copyBuf)
	for {
		b := &lineBatch{data: batchBufs.Get().([]byte), done: make(chan struct{})}
		for {
			line, err := br.ReadSlice('\n')
			b.data = append(b.data, line...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				b.err = err
				break
			}
			if len(b.data) >= batchSize {
				break
			}
		}
		if !emit(b) || b.err != nil {
			return
		}
	}
}

// parLines is a lineSource that rewrites runs of lines on several goroutines
// and yields them in their order. fn must be safe for concurrent use.
type parLines struct {
	queue chan *lineBatch
	stop  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup // the reader and the workers
	cur   *lineBatch
	i     int
	err   error
}

func newParLines(r io.Reader, fn lineFn, n int) *parLines {
	p := &parLines{queue: make(chan *lineBatch, 2*n), stop: make(chan struct{})}
	work := make(chan *lineBatch, 2*n)
	p.wg.Add(n + 1)
	for range n {
		go func() {
			defer p.wg.Done()
			for b := range work {
				b.run(fn)
				close(b.done)
			}
		}()
	}
	go func() {
		defer p.wg.Done()
		defer close(work)
		defer close(p.queue)
		readBatches(r, func(b *lineBatch) bool {
			select {
			case work <- b:
			case <-p.stop:
				return false
			}
			select {
			case p.queue <- b:
				return true
			case <-p.stop:
				return false
			}
		})
	}()
	return p
}

func (p *parLines) next() ([]byte, any, int, error) {
	for {
		if b := p.cur; b != nil && p.i < len(b.ends) {
			start := 0
			if p.i > 0 {
				start = b.ends[p.i-1]
			}
			out, note, n := b.out[p.i], b.notes[p.i], b.ends[p.i]-start
			p.i++
			return out, note, n, nil
		}
		if p.err != nil {
			return nil, nil, 0, p.err
		}
		if p.cur != nil && p.cur.err != nil {
			p.err = p.cur.err
			p.cur.free()
			continue
		}
		b, ok := <-p.queue
		if !ok {
			p.err = io.ErrUnexpectedEOF // stopped
			continue
		}
		<-b.done
		if p.cur != nil {
			p.cur.free() // its last line was handed out a call ago
		}
		p.cur, p.i = b, 0
	}
}

// close stops the goroutines and waits for them, so nothing reads the stream
// (or reports its progress) after it; the source may be left unread.
func (p *parLines) close() {
	p.once.Do(func() { close(p.stop) })
	p.wg.Wait()
}

// parallelFrom is the size from which a stream is worth several goroutines.
const parallelFrom = 4 * batchSize

// newLines is the lineSource for a stream of size bytes: on several goroutines
// when fn has work to do, the stream is large and there is more than one CPU.
func newLines(r io.Reader, fn lineFn, size int64) lineSource {
	if n := workers(); fn != nil && n > 1 && size >= parallelFrom {
		return newParLines(r, fn, n)
	}
	return newSeqLines(r, fn, size)
}
