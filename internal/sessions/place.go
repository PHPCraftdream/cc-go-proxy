package sessions

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// copyBuf is the I/O chunk size; transcripts can be gigabytes.
const copyBuf = 1 << 20

// errDiverged aborts a copy whose source differs from what the destination
// already holds.
var errDiverged = errors.New("sessions: destination differs from the source")

// syncFile writes src to dst under the append-only rule described at Copy,
// streaming: nothing is held in memory but one chunk. jsonl files are cut to
// their last complete line (the source may be a live session) and optionally
// stripped of thinking blocks. prog, when set, receives the bytes of src
// consumed so far (throttled by opt.ProgressEvery).
func syncFile(src, dst string, jsonl bool, opt Options, prog func(done, total int64)) (Status, int, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	size := info.Size()
	if jsonl {
		if size, err = completeLen(f, size); err != nil {
			return 0, 0, err
		}
	}

	p, err := newPlacer(dst, info.ModTime())
	if err != nil {
		return 0, 0, err
	}
	defer p.close()

	var in io.Reader = io.NewSectionReader(f, 0, size)
	if prog != nil {
		every := opt.ProgressEvery
		if every == 0 {
			every = defaultProgressEvery
		}
		in = &progressReader{r: in, total: size, every: every, last: time.Now(), fn: prog}
	}
	if fn := lineRewrite(opt, jsonl, src); fn != nil {
		in = newSwapReader(in, fn)
	}
	removed := 0
	if jsonl && opt.StripThinking {
		bw := bufio.NewWriterSize(p, copyBuf)
		if removed, err = StripThinkingStream(in, bw); err == nil {
			err = bw.Flush()
		}
	} else {
		_, err = io.CopyBuffer(p, in, make([]byte, copyBuf))
	}
	if errors.Is(err, errDiverged) {
		return Diverged, removed, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", src, err)
	}
	st, err := p.finish()
	return st, removed, err
}

// place is syncFile plus Options.Force: a destination that diverged from the
// source or is ahead of it is renamed to <dst>.bak-<time> and written afresh.
// backup is that path, empty when nothing was replaced.
func place(src, dst string, jsonl bool, opt Options, prog func(done, total int64)) (st Status, removed int, backup string, err error) {
	st, removed, err = syncFile(src, dst, jsonl, opt, prog)
	if err != nil || !opt.Force || (st != Diverged && st != Ahead) {
		return st, removed, "", err
	}
	stamp := time.Now().Format("20060102-150405")
	backup = dst + ".bak-" + stamp
	for i := 1; ; i++ {
		if _, serr := os.Lstat(backup); errors.Is(serr, fs.ErrNotExist) {
			break
		}
		backup = fmt.Sprintf("%s.bak-%s-%d", dst, stamp, i)
	}
	if err = os.Rename(dst, backup); err != nil {
		return 0, 0, "", err
	}
	if _, removed, err = syncFile(src, dst, jsonl, opt, prog); err != nil {
		_ = os.Rename(backup, dst) // nothing was written: put the old file back
		return 0, 0, "", err
	}
	return Replaced, removed, backup, nil
}

// forkID is the id a diverged session is written under: derived from the
// original, so the next fork of it lands on the same file and extends it.
func forkID(id string) string { return uuidOf("ccgp fork " + id) }

// forkUUID is the id the fork fid has in place of the uuid u (of a message, a
// prompt, …): a fork that kept the original's message ids would be the same
// conversation to Claude Code, which lists only one of the two.
func forkUUID(fid, u string) string { return uuidOf("ccgp fork " + fid + " " + u) }

// uuidOf derives a version 4 layout id from s, as Claude Code's own ids.
func uuidOf(s string) string {
	h := sha256.Sum256([]byte(s))
	h[6] = h[6]&0x0f | 0x40
	h[8] = h[8]&0x3f | 0x80
	x := hex.EncodeToString(h[:16])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

// relocatedKey is the field of a relocated line that names the directory.
var relocatedKey = []byte(`"relocatedCwd":`)

// isMarker reports whether a transcript line is of type want, whatever the
// order of its fields; a message quoting such a line is not one.
func isMarker(line []byte, want string) bool {
	if !bytes.Contains(line, []byte(`"`+want+`"`)) {
		return false
	}
	var v struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(line, &v) == nil && v.Type == want
}

// lineRewrite is what a copy changes in the lines of src, nil for nothing:
//   - a session written into the folder of opt.home (Session.Into) says it was
//     relocated there: Claude Code lists a session under the last relocatedCwd
//     of its transcript, before the cwd of its first lines, and the worktree
//     the source names would hide it from the directory's /resume;
//   - a fork gets its own ids, and no owner: the session-owner lines name the
//     account of the source's Claude Code, and Claude Code stamps a transcript
//     that has none with the account that opens it.
func lineRewrite(opt Options, jsonl bool, src string) func([]byte) []byte {
	ids := opt.rewrite
	if !jsonl && !rewritable(src) {
		ids = nil
	}
	var home []byte
	if jsonl && opt.home != "" {
		home, _ = json.Marshal(opt.home)
	}
	if ids == nil && home == nil {
		return nil
	}
	return func(line []byte) []byte {
		if jsonl && ids != nil && isMarker(line, "session-owner") {
			return nil
		}
		if home != nil && isMarker(line, "relocated") {
			line = setRelocated(line, home)
		}
		if ids != nil {
			line = ids.line(line)
		}
		return line
	}
}

// setRelocated puts the JSON string home in place of the line's relocatedCwd.
func setRelocated(line, home []byte) []byte {
	i := bytes.Index(line, relocatedKey)
	if i < 0 {
		return line
	}
	i += len(relocatedKey)
	var old string
	dec := json.NewDecoder(bytes.NewReader(line[i:]))
	if dec.Decode(&old) != nil {
		return line
	}
	out := append(append([]byte{}, line[:i]...), home...)
	return append(out, line[i+int(dec.InputOffset()):]...)
}

// swapReader passes a file through fn, line by line; a line fn returns empty
// is left out.
type swapReader struct {
	br  *bufio.Reader
	fn  func([]byte) []byte
	buf []byte
	err error
}

func newSwapReader(r io.Reader, fn func([]byte) []byte) *swapReader {
	return &swapReader{br: bufio.NewReaderSize(r, copyBuf), fn: fn}
}

func (s *swapReader) Read(p []byte) (int, error) {
	for len(s.buf) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		var line []byte
		line, s.err = s.br.ReadBytes('\n')
		s.buf = s.fn(line)
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// progressReader reports how much of a stream was read, at most once per interval.
type progressReader struct {
	r     io.Reader
	total int64
	done  int64
	every time.Duration
	last  time.Time
	fn    func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if n > 0 {
		if now := time.Now(); p.every < 0 || now.Sub(p.last) >= p.every {
			p.last = now
			p.fn(p.done, p.total)
		}
	}
	return n, err
}

// completeLen returns the length of the file's prefix that ends at its last
// newline; a file without any newline is taken whole.
func completeLen(f *os.File, size int64) (int64, error) {
	buf := make([]byte, 64<<10)
	for end := size; end > 0; {
		start := max(0, end-int64(len(buf)))
		chunk := buf[:end-start]
		if n, err := f.ReadAt(chunk, start); n < len(chunk) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return size, nil
}

// placer is the io.Writer behind syncFile. It checks the stream against the
// destination's current content as it arrives: any difference aborts with
// errDiverged before anything is modified; bytes beyond the existing content
// are appended (or, for a new file, written to a temp file renamed in finish).
type placer struct {
	dst     string
	mtime   time.Time
	old     *os.File // existing destination, nil when absent
	oldSize int64
	pos     int64 // stream bytes seen so far
	cmp     []byte
	out     *os.File
	tmp     string // temp file of a new destination until finish
	wrote   bool   // bytes beyond the old content were written
}

func newPlacer(dst string, mtime time.Time) (*placer, error) {
	p := &placer{dst: dst, mtime: mtime}
	f, err := os.Open(dst)
	switch {
	case err == nil:
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		p.old, p.oldSize = f, info.Size()
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, err
	}
	return p, nil
}

func (p *placer) Write(b []byte) (int, error) {
	n := len(b)
	if p.old != nil && p.pos < p.oldSize {
		m := min(int64(len(b)), p.oldSize-p.pos)
		if int64(cap(p.cmp)) < m {
			p.cmp = make([]byte, m)
		}
		got := p.cmp[:m]
		if r, err := p.old.ReadAt(got, p.pos); r < len(got) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if !bytes.Equal(got, b[:m]) {
			return 0, errDiverged
		}
		p.pos += m
		b = b[m:]
	}
	if len(b) == 0 {
		return n, nil
	}
	if p.out == nil {
		if err := p.openOut(); err != nil {
			return 0, err
		}
	}
	if _, err := p.out.Write(b); err != nil {
		return 0, err
	}
	p.pos += int64(len(b))
	p.wrote = true
	return n, nil
}

func (p *placer) openOut() error {
	var err error
	if p.old != nil {
		p.out, err = os.OpenFile(p.dst, os.O_WRONLY|os.O_APPEND, 0)
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p.dst), 0o700); err != nil {
		return err
	}
	p.tmp = p.dst + ".ccgp-tmp"
	p.out, err = os.OpenFile(p.tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	return err
}

// finish completes the destination and reports what was done to it.
func (p *placer) finish() (Status, error) {
	if p.old == nil { // new file, possibly empty
		if p.out == nil {
			if err := p.openOut(); err != nil {
				return 0, err
			}
		}
		if err := p.out.Close(); err != nil {
			return 0, err
		}
		p.out = nil
		if err := os.Chtimes(p.tmp, p.mtime, p.mtime); err != nil {
			return 0, err
		}
		if err := os.Rename(p.tmp, p.dst); err != nil {
			return 0, err
		}
		p.tmp = ""
		return Copied, nil
	}
	switch {
	case p.wrote:
		if err := p.out.Close(); err != nil {
			return 0, err
		}
		p.out = nil
		return Updated, os.Chtimes(p.dst, p.mtime, p.mtime)
	case p.pos == p.oldSize:
		return Unchanged, nil
	}
	return Ahead, nil
}

// close releases the handles and removes an unfinished temp file.
func (p *placer) close() {
	if p.old != nil {
		_ = p.old.Close()
	}
	if p.out != nil {
		_ = p.out.Close()
	}
	if p.tmp != "" {
		_ = os.Remove(p.tmp)
	}
}
