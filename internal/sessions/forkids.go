package sessions

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// forkIDs maps every id of a session to the one its fork has, the same way in
// every line, file and file name and on every run: links between messages,
// tool calls and their results, subagents and their files all hold, and a
// rerun extends the fork instead of diverging from it. A fork that kept any id
// of the original would be the same thing to Claude Code in places.
//
// Ids: the session id (to the fork id), the uuids of the session (messages,
// prompts, …: every uuid that is the value of a field somewhere in it), the
// API's toolu_/msg_/req_ ids, and the short ids of subagents (a + hex) and
// background tasks found in id fields and in the names of the session's files.
// Each is replaced wherever it stands, text included; a uuid that is only
// mentioned in text (a link to something else) is not the session's and stays.
type forkIDs struct {
	old, fid string
	uuids    map[string]bool // the session's uuids, lower case
	short    map[string]bool // short ids, to be replaced wherever they stand alone
	// slugs maps each slug of the session (the name of its plan file) to the
	// fork's: one plan file for two sessions would be written by both.
	slugs map[string]string
	// shortAs is short mapped once (prepare); shape has bit n set for the first
	// byte of a short id of length n, so most words need no map lookup.
	shortAs map[string]string
	shape   [256]uint32
}

var (
	// shortText is how Claude Code names a background task or a subagent in
	// text, often long before the id field: "with ID: b…", ".../tasks/b….output",
	// "<task-id>b…</task-id>", "agentId: a…".
	shortText = regexp.MustCompile(`(?:with ID: |<task-id>|agentId: |tasks[\\/]+)([A-Za-z0-9]{6,24})(?:</task-id>|\.output|[^A-Za-z0-9]|$)`)
	// shortField is a field holding the short id of a subagent or a task.
	shortField = regexp.MustCompile(`"(?:agentId|agent_id|[A-Za-z]*[Tt]ask_?[Ii]d|task_id|bash_id|bashId|shell_id|shellId)"\s*:\s*"([A-Za-z0-9]{6,24})"`)
	shortName  = regexp.MustCompile(`^(?:agent-)?([A-Za-z0-9]{6,24})(?:\.[A-Za-z]+)+$`)
	// letterTask is a background task id of letters only (b + 8).
	letterTask = regexp.MustCompile(`^b[a-z]{8}$`)
)

// newForkIDs collects the short ids of the session: its transcript and the
// files of its folder side (which may not exist).
func newForkIDs(old, fid, transcript, side string) (*forkIDs, error) {
	m := &forkIDs{old: old, fid: fid, uuids: map[string]bool{}, short: map[string]bool{}, slugs: map[string]string{}}
	if err := m.scan(transcript); err != nil {
		return nil, err
	}
	err := filepath.WalkDir(side, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == side {
				return filepath.SkipDir
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		// Claude Code names the files of tool-results/ and subagents/ by id
		// (a task's output may be named by an id of letters only, mentioned
		// nowhere else); elsewhere a name is an id only with a digit in it.
		if sm := shortName.FindStringSubmatch(d.Name()); sm != nil {
			dir := filepath.Base(filepath.Dir(p))
			if dir == "tool-results" || dir == "subagents" || strings.IndexFunc(sm[1], isDigit) >= 0 {
				m.add(sm[1])
			}
		}
		if rewritable(p) || strings.HasSuffix(p, ".jsonl") {
			return m.scan(p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	m.prepare()
	return m, nil
}

// prepare maps every short id once; line is then free of writes and safe for
// concurrent use.
func (m *forkIDs) prepare() {
	m.shortAs = make(map[string]string, len(m.short))
	for id := range m.short {
		m.shortAs[id] = m.mapShort(id)
		if n := len(id); n < 32 {
			m.shape[id[0]] |= 1 << n
		}
	}
}

// isShort reports whether b is a short id of the session.
func (m *forkIDs) isShort(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	if m.shortAs != nil && (len(b) >= 32 || m.shape[b[0]]&(1<<len(b)) == 0) {
		return false
	}
	return m.short[string(b)]
}

// add keeps a short id: letters and digits mixed, or a task id of letters
// (b + 8). A number (a task list's 100001) is not one, nor a word a field may
// hold ("deployment"), which would be replaced all over the text.
func (m *forkIDs) add(id string) {
	letters := strings.IndexFunc(id, func(r rune) bool { return !isDigit(r) }) >= 0
	digits := strings.IndexFunc(id, isDigit) >= 0
	if letters && digits || letterTask.MatchString(id) {
		m.short[id] = true
	}
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// scan collects the ids of a file; a large one is read on several goroutines.
func (m *forkIDs) scan(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	n := workers()
	if info, err := f.Stat(); err != nil || info.Size() < parallelFrom || n < 2 {
		br := bufio.NewReaderSize(f, copyBuf)
		var long []byte
		for {
			line, err := readLine(br, &long)
			m.scanLine(line)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	parts := make([]*forkIDs, n)
	work := make(chan *lineBatch, 2*n)
	var wg sync.WaitGroup
	for i := range parts {
		part := &forkIDs{fid: m.fid, uuids: map[string]bool{}, short: map[string]bool{}, slugs: map[string]string{}}
		parts[i] = part
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range work {
				for rest := b.data; len(rest) > 0; {
					end := len(rest)
					if i := bytes.IndexByte(rest, '\n'); i >= 0 {
						end = i + 1
					}
					part.scanLine(rest[:end])
					rest = rest[end:]
				}
				b.free()
			}
		}()
	}
	var rerr error
	readBatches(f, func(b *lineBatch) bool {
		work <- b
		if b.err != nil && !errors.Is(b.err, io.EOF) {
			rerr = b.err
		}
		return true
	})
	close(work)
	wg.Wait()
	for _, p := range parts {
		for k := range p.uuids {
			m.uuids[k] = true
		}
		for k := range p.short {
			m.short[k] = true
		}
		for k, v := range p.slugs {
			m.slugs[k] = v
		}
	}
	return rerr
}

// scanLine collects the ids of one line.
func (m *forkIDs) scanLine(line []byte) {
	if hasShortField(line) {
		for _, sm := range shortField.FindAllSubmatch(line, -1) {
			m.add(string(sm[1]))
		}
	}
	collectUUIDValues(line, m.uuids)
	if bytes.Contains(line, []byte(`"slug"`)) {
		for _, sm := range slugField.FindAllSubmatch(line, -1) {
			if s := string(sm[1]); m.slugs[s] == "" {
				m.slugs[s] = s + "-" + m.fid[:min(8, len(m.fid))]
			}
		}
	}
	// Named in text before any field does: kept from the first mention, so
	// a later run, when the field has come, maps the earlier lines the same.
	if bytes.Contains(line, []byte("with ID: ")) || bytes.Contains(line, []byte("<task-id>")) ||
		bytes.Contains(line, []byte("agentId: ")) || bytes.Contains(line, []byte(".output")) {
		for _, sm := range shortText.FindAllSubmatch(line, -1) {
			m.add(string(sm[1]))
		}
	}
}

// line gives every id of b its fork id.
func (m *forkIDs) line(b []byte) []byte {
	if m.old != "" && bytes.Contains(b, []byte(m.old)) {
		b = bytes.ReplaceAll(b, []byte(m.old), []byte(m.fid))
	}
	if len(m.uuids) > 0 {
		b = m.mapUUIDs(b)
	}
	b = m.mapAPIIDs(b)
	if len(m.short) > 0 {
		b = m.shortIDs(b)
	}
	for old, as := range m.slugs {
		if bytes.Contains(b, []byte(old)) {
			b = replaceWord(b, old, as)
		}
	}
	return b
}

// replaceWord replaces old where it stands alone: not inside a longer run of
// letters, digits, '-' and '_' (another slug, or the fork's own, which begins
// with it).
func replaceWord(b []byte, old, as string) []byte {
	word := func(c byte) bool { return isAlnum(c) || c == '-' || c == '_' }
	var out []byte
	last := 0
	for i := 0; ; {
		j := bytes.Index(b[i:], []byte(old))
		if j < 0 {
			break
		}
		at, end := i+j, i+j+len(old)
		i = end
		if at > 0 && word(b[at-1]) || end < len(b) && word(b[end]) {
			continue
		}
		out = append(append(out, b[last:at]...), as...)
		last = end
	}
	if out == nil {
		return b
	}
	return append(out, b[last:]...)
}

// name gives the ids in a path of the session's folder their fork ids.
func (m *forkIDs) name(rel string) string { return string(m.line([]byte(rel))) }

// shortIDs replaces each run of letters and digits that is a known short id. A
// run glued to a JSON escape (\n, \t, …) is the id after it.
func (m *forkIDs) shortIDs(b []byte) []byte {
	var out []byte
	last := 0
	for i := 0; i < len(b); {
		if !isAlnum(b[i]) {
			i++
			continue
		}
		j := i
		for j < len(b) && isAlnum(b[j]) {
			j++
		}
		at := i
		if i > 0 && b[i-1] == '\\' && strings.IndexByte("nrtbf", b[i]) >= 0 && !m.isShort(b[i:j]) {
			at = i + 1
		}
		if n := j - at; n >= 6 && n <= 24 && m.isShort(b[at:j]) {
			out = append(out, b[last:at]...)
			out = append(out, m.shortFor(string(b[at:j]))...)
			last = j
		}
		i = j
	}
	if out == nil {
		return b
	}
	return append(out, b[last:]...)
}

// shortFor is the fork's id for the short id id.
func (m *forkIDs) shortFor(id string) string {
	if as, ok := m.shortAs[id]; ok {
		return as
	}
	return m.mapShort(id)
}

// mapShort keeps the first character (a for an agent, b for a task) and the
// alphabet of the rest.
func (m *forkIDs) mapShort(id string) string {
	alphabet := base36
	if strings.Trim(id[1:], "0123456789abcdef") == "" {
		alphabet = hexDigits
	}
	return id[:1] + derive(m.fid, id, alphabet, len(id)-1)
}

const (
	base62    = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	base36    = "0123456789abcdefghijklmnopqrstuvwxyz"
	hexDigits = "0123456789abcdef"
)

// derive is n characters of alphabet made from the fork id and the old id.
func derive(fid, old, alphabet string, n int) string {
	out := make([]byte, 0, n)
	for c := byte(0); len(out) < n; c++ {
		h := sha256.Sum256([]byte("ccgp fork " + fid + " " + old + " " + string(rune('0'+c))))
		for _, x := range h {
			if len(out) == n {
				break
			}
			out = append(out, alphabet[int(x)%len(alphabet)])
		}
	}
	return string(out)
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// rewritable tells the files of a session's folder whose text holds ids: JSON,
// text and Markdown. Others are copied as they are.
func rewritable(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json", ".txt", ".md", ".log":
		return true
	}
	return false
}
