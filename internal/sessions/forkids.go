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
}

var (
	anyUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// apiID is an id of the Anthropic API: a prefix, then 24 base62 characters
	// starting with 0.
	apiID = regexp.MustCompile(`(?:srvtoolu|toolu|msg|req)_0[0-9A-Za-z]{23}`)
	// codexID is an id of the Responses API, which a session run through ccgp
	// on Codex holds: tool calls call_<32 hex>, replies resp_<50 hex>, items
	// fc_/rs_. 20 to 64 letters and digits, a digit among them (call_something
	// in code is a name).
	codexID = regexp.MustCompile(`(?:call|resp|fc|rs)_[0-9A-Za-z]{20,64}`)
	// shortText is how Claude Code names a background task or a subagent in
	// text, often long before the id field: "with ID: b…", ".../tasks/b….output",
	// "<task-id>b…</task-id>", "agentId: a…".
	shortText = regexp.MustCompile(`(?:with ID: |<task-id>|agentId: |tasks[\\/]+)([A-Za-z0-9]{6,24})(?:</task-id>|\.output|[^A-Za-z0-9]|$)`)
	// shortField is a field holding the short id of a subagent or a task.
	shortField = regexp.MustCompile(`"(?:agentId|agent_id|[A-Za-z]*[Tt]ask_?[Ii]d|task_id|bash_id|bashId|shell_id|shellId)"\s*:\s*"([A-Za-z0-9]{6,24})"`)
	shortName  = regexp.MustCompile(`^(?:agent-)?([A-Za-z0-9]{6,24})(?:\.[A-Za-z]+)+$`)
	// uuidValue is a uuid that is a whole JSON string: a field's value.
	uuidValue = regexp.MustCompile(`"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`)
	// letterTask is a background task id of letters only (b + 8).
	letterTask = regexp.MustCompile(`^b[a-z]{8}$`)
)

// newForkIDs collects the short ids of the session: its transcript and the
// files of its folder side (which may not exist).
func newForkIDs(old, fid, transcript, side string) (*forkIDs, error) {
	m := &forkIDs{old: old, fid: fid, uuids: map[string]bool{}, short: map[string]bool{}}
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
	return m, nil
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

func (m *forkIDs) scan(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReaderSize(f, copyBuf)
	for {
		line, err := br.ReadBytes('\n')
		if bytes.Contains(line, []byte(`d"`)) || bytes.Contains(line, []byte(`D"`)) {
			for _, sm := range shortField.FindAllSubmatch(line, -1) {
				m.add(string(sm[1]))
			}
		}
		for _, sm := range uuidValue.FindAllSubmatch(line, -1) {
			m.uuids[strings.ToLower(string(sm[1]))] = true
		}
		// Named in text before any field does: kept from the first mention, so
		// a later run, when the field has come, maps the earlier lines the same.
		if bytes.Contains(line, []byte("with ID: ")) || bytes.Contains(line, []byte("<task-id>")) ||
			bytes.Contains(line, []byte("agentId: ")) || bytes.Contains(line, []byte(".output")) {
			for _, sm := range shortText.FindAllSubmatch(line, -1) {
				m.add(string(sm[1]))
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// line gives every id of b its fork id.
func (m *forkIDs) line(b []byte) []byte {
	if m.old != "" && bytes.Contains(b, []byte(m.old)) {
		b = bytes.ReplaceAll(b, []byte(m.old), []byte(m.fid))
	}
	b = anyUUID.ReplaceAllFunc(b, func(u []byte) []byte {
		s := strings.ToLower(string(u))
		if !m.uuids[s] {
			return u
		}
		return []byte(forkUUID(m.fid, s))
	})
	b = replaceAt(b, apiID.FindAllIndex(b, -1), func(id []byte) []byte {
		i := bytes.IndexByte(id, '_') + 3 // prefix_01 stays
		return append(append([]byte{}, id[:i]...), derive(m.fid, string(id), base62, len(id)-i)...)
	})
	b = replaceAt(b, codexID.FindAllIndex(b, -1), func(id []byte) []byte {
		i := bytes.IndexByte(id, '_') + 1
		body := id[i:]
		if bytes.IndexFunc(body, func(r rune) bool { return r >= '0' && r <= '9' }) < 0 {
			return id // a name, not an id
		}
		alphabet := base62
		if len(bytes.Trim(body, hexDigits)) == 0 {
			alphabet = hexDigits
		}
		return append(append([]byte{}, id[:i]...), derive(m.fid, string(id), alphabet, len(body))...)
	})
	if len(m.short) > 0 {
		b = m.shortIDs(b)
	}
	return b
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
		if i > 0 && b[i-1] == '\\' && strings.IndexByte("nrtbf", b[i]) >= 0 && !m.short[string(b[i:j])] {
			at = i + 1
		}
		if n := j - at; n >= 6 && n <= 24 && m.short[string(b[at:j])] {
			out = append(out, b[last:at]...)
			out = append(out, m.mapShort(string(b[at:j]))...)
			last = j
		}
		i = j
	}
	if out == nil {
		return b
	}
	return append(out, b[last:]...)
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

// replaceAt replaces the matches at idx that stand alone: no letter or digit
// right after, none right before (but the letter of a JSON escape: \ncall_…).
func replaceAt(b []byte, idx [][]int, fn func([]byte) []byte) []byte {
	if len(idx) == 0 {
		return b
	}
	var out []byte
	last := 0
	for _, ix := range idx {
		if ix[1] < len(b) && isAlnum(b[ix[1]]) {
			continue
		}
		if s := ix[0]; s > 0 && isAlnum(b[s-1]) && (s < 2 || b[s-2] != '\\' || strings.IndexByte("nrtbf", b[s-1]) < 0) {
			continue
		}
		out = append(out, b[last:ix[0]]...)
		out = append(out, fn(b[ix[0]:ix[1]])...)
		last = ix[1]
	}
	return append(out, b[last:]...)
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
