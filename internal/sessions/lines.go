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

// lineSource yields the lines of a stream, each passed through a rewrite. A
// line is valid until the next call. n is the line's length in the stream; out
// is empty when the rewrite drops it. err comes with the last line or after it.
type lineSource interface {
	next() (out []byte, n int, err error)
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
	fn   func([]byte) []byte // nil: lines as they are
	long []byte
}

func newSeqLines(r io.Reader, fn func([]byte) []byte) *seqLines {
	return &seqLines{br: bufio.NewReaderSize(r, copyBuf), fn: fn}
}

func (s *seqLines) next() ([]byte, int, error) {
	line, err := readLine(s.br, &s.long)
	n := len(line)
	if n > 0 && s.fn != nil {
		line = s.fn(line)
	}
	return line, n, err
}

func (s *seqLines) close() {}

// lineBatch is a run of whole lines of a stream and what fn made of them.
type lineBatch struct {
	data []byte
	err  error    // of the stream, after data
	ends []int    // end of each line in data
	out  [][]byte // each line rewritten
	done chan struct{}
}

func (b *lineBatch) run(fn func([]byte) []byte) {
	for start := 0; start < len(b.data); {
		end := len(b.data)
		if i := bytes.IndexByte(b.data[start:], '\n'); i >= 0 {
			end = start + i + 1
		}
		b.ends = append(b.ends, end)
		b.out = append(b.out, fn(b.data[start:end]))
		start = end
	}
}

// readBatches cuts r into runs of whole lines of about batchSize bytes and
// hands them to emit, in order; the last one carries the stream's error
// (io.EOF at its end). It stops when emit returns false.
func readBatches(r io.Reader, emit func(*lineBatch) bool) {
	br := bufio.NewReaderSize(r, copyBuf)
	for {
		b := &lineBatch{data: make([]byte, 0, batchSize+copyBuf), done: make(chan struct{})}
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
	cur   *lineBatch
	i     int
	err   error
}

func newParLines(r io.Reader, fn func([]byte) []byte, n int) *parLines {
	p := &parLines{queue: make(chan *lineBatch, 2*n), stop: make(chan struct{})}
	work := make(chan *lineBatch, 2*n)
	for range n {
		go func() {
			for b := range work {
				b.run(fn)
				close(b.done)
			}
		}()
	}
	go func() {
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

func (p *parLines) next() ([]byte, int, error) {
	for {
		if b := p.cur; b != nil && p.i < len(b.ends) {
			start := 0
			if p.i > 0 {
				start = b.ends[p.i-1]
			}
			out, n := b.out[p.i], b.ends[p.i]-start
			p.i++
			return out, n, nil
		}
		if p.err != nil {
			return nil, 0, p.err
		}
		if p.cur != nil && p.cur.err != nil {
			p.err = p.cur.err
			continue
		}
		b, ok := <-p.queue
		if !ok {
			p.err = io.ErrUnexpectedEOF // stopped
			continue
		}
		<-b.done
		p.cur, p.i = b, 0
	}
}

// close stops the goroutines; the source may be left unread.
func (p *parLines) close() { p.once.Do(func() { close(p.stop) }) }

// newLines is the lineSource for a stream: on several goroutines when fn has
// work to do and there is more than one CPU.
func newLines(r io.Reader, fn func([]byte) []byte) lineSource {
	if n := workers(); fn != nil && n > 1 {
		return newParLines(r, fn, n)
	}
	return newSeqLines(r, fn)
}
