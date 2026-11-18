// Package jsonl reads, rewrites and trims the JSON-lines transcripts of Claude Code
// sessions: lines worked on in parallel, thinking blocks stripped, a failed end found.

package jsonl

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"runtime"
	"sync"
)

// CopyBuf is the I/O chunk size; transcripts can be gigabytes.
const CopyBuf = 1 << 20

// batchSize is about how much of a stream one worker takes at a time.
const batchSize = 1 << 20

// Workers is how many lines are worked on at once.
func Workers() int { return min(runtime.GOMAXPROCS(0), 8) }

// LineFn is the work on one line that needs no state: it may run on any
// goroutine. It returns the line's new form and a note for the work that
// follows in order (nil: none).
type LineFn func(line []byte) ([]byte, any)

// Source yields the lines of a stream, each passed through a LineFn. A
// line is valid until the next call. n is the line's length in the stream; out
// is empty when the LineFn drops it. err comes with the last line or after it.
type Source interface {
	Next() (out []byte, note any, n int, err error)
	Close()
}

// ReadLine reads up to and including the next '\n', or to the end, without a
// copy for lines that fit br's buffer; a longer one is put together in *long.
func ReadLine(br *bufio.Reader, long *[]byte) ([]byte, error) {
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

// seqLines is a Source working line after line.
type seqLines struct {
	br   *bufio.Reader
	fn   LineFn // nil: lines as they are
	long []byte
}

// NewSeqLines reads r, which holds about size bytes (0: unknown).
func NewSeqLines(r io.Reader, fn LineFn, size int64) *seqLines {
	buf := CopyBuf
	if size > 0 && size < CopyBuf {
		buf = max(int(size), 4<<10)
	}
	return &seqLines{br: bufio.NewReaderSize(r, buf), fn: fn}
}

func (s *seqLines) Next() ([]byte, any, int, error) {
	line, err := ReadLine(s.br, &s.long)
	n := len(line)
	var note any
	if n > 0 && s.fn != nil {
		line, note = s.fn(line)
	}
	return line, note, n, err
}

func (s *seqLines) Close() {}

// LineBatch is a run of whole lines of a stream and what fn made of them.
type LineBatch struct {
	Data  []byte
	Err   error    // of the stream, after Data
	ends  []int    // end of each line in data
	out   [][]byte // each line rewritten
	notes []any
	done  chan struct{}
}

func (b *LineBatch) run(fn LineFn) {
	for start := 0; start < len(b.Data); {
		end := len(b.Data)
		if i := bytes.IndexByte(b.Data[start:], '\n'); i >= 0 {
			end = start + i + 1
		}
		out, note := fn(b.Data[start:end])
		b.ends = append(b.ends, end)
		b.out = append(b.out, out)
		b.notes = append(b.notes, note)
		start = end
	}
}

// batchBufs recycles the buffers of batches: a large file has thousands.
var batchBufs = sync.Pool{New: func() any { return make([]byte, 0, batchSize+CopyBuf) }}

// Free gives the batch's buffer back; its lines must not be used after.
func (b *LineBatch) Free() {
	if cap(b.Data) <= 2*(batchSize+CopyBuf) { // one grown by a huge line is not kept
		batchBufs.Put(b.Data[:0]) //nolint:staticcheck // a slice header is small
	}
	b.Data, b.out, b.notes = nil, nil, nil
}

// ReadBatches cuts r into runs of whole lines of about batchSize bytes and
// hands them to emit, in order; the last one carries the stream's error
// (io.EOF at its end). It stops when emit returns false.
func ReadBatches(r io.Reader, emit func(*LineBatch) bool) {
	br := bufio.NewReaderSize(r, CopyBuf)
	for {
		b := &LineBatch{Data: batchBufs.Get().([]byte), done: make(chan struct{})}
		for {
			line, err := br.ReadSlice('\n')
			b.Data = append(b.Data, line...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				b.Err = err
				break
			}
			if len(b.Data) >= batchSize {
				break
			}
		}
		if !emit(b) || b.Err != nil {
			return
		}
	}
}

// parLines is a Source that rewrites runs of lines on several goroutines
// and yields them in their order. fn must be safe for concurrent use.
type parLines struct {
	queue chan *LineBatch
	stop  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup // the reader and the workers
	cur   *LineBatch
	i     int
	err   error
}

func newParLines(r io.Reader, fn LineFn, n int) *parLines {
	p := &parLines{queue: make(chan *LineBatch, 2*n), stop: make(chan struct{})}
	work := make(chan *LineBatch, 2*n)
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
		ReadBatches(r, func(b *LineBatch) bool {
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

func (p *parLines) Next() ([]byte, any, int, error) {
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
		if p.cur != nil && p.cur.Err != nil {
			p.err = p.cur.Err
			p.cur.Free()
			continue
		}
		b, ok := <-p.queue
		if !ok {
			p.err = io.ErrUnexpectedEOF // stopped
			continue
		}
		<-b.done
		if p.cur != nil {
			p.cur.Free() // its last line was handed out a call ago
		}
		p.cur, p.i = b, 0
	}
}

// close stops the goroutines and waits for them, so nothing reads the stream
// (or reports its progress) after it; the source may be left unread.
func (p *parLines) Close() {
	p.once.Do(func() { close(p.stop) })
	p.wg.Wait()
}

// ParallelFrom is the size from which a stream is worth several goroutines.
const ParallelFrom = 4 * batchSize

// NewLines is the Source for a stream of size bytes: on several goroutines
// when fn has work to do, the stream is large and there is more than one CPU.
func NewLines(r io.Reader, fn LineFn, size int64) Source {
	if n := Workers(); fn != nil && n > 1 && size >= ParallelFrom {
		return newParLines(r, fn, n)
	}
	return NewSeqLines(r, fn, size)
}
