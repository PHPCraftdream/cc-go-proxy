package sessions

import (
	"bytes"
	"encoding/json"
	"io"
)

// A transcript can end in turns that never got an answer: Claude Code writes an
// API error in place of the reply (a usage limit, a server error), often once
// per tick of a scheduled prompt that kept firing. A copy of the session leaves
// that end out, so the copy goes on from its last real exchange.

var (
	failedTailWindow int64 = 8 << 20  // first look at the transcript's last bytes
	failedTailMax    int64 = 64 << 20 // a failed end longer than this is kept
)

// apiErrorMark is in the raw text of every API error line.
var apiErrorMark = []byte(`"isApiErrorMessage":true`)

// failedTurnSystem are the system lines a failed turn leaves.
var failedTurnSystem = map[string]bool{
	"turn_duration":       true,
	"scheduled_task_fire": true,
	"api_error":           true,
	"stop_hook_summary":   true,
	"away_summary":        true,
}

type tailKind int

const (
	tailUseful tailKind = iota // part of the conversation: ends the failed tail
	tailFailed                 // an API error in place of a reply
	tailNoise                  // a request record or a turn of a failed run
	tailState                  // session state written after a prompt (title, mode, ...)
)

// classifyTail tells what a line is to failedTail. A line it cannot read is
// taken as part of the conversation.
func classifyTail(line []byte) tailKind {
	if len(bytes.TrimSpace(line)) == 0 {
		return tailState
	}
	var v struct {
		Type       string `json:"type"`
		Subtype    string `json:"subtype"`
		UUID       string `json:"uuid"`
		IsMeta     bool   `json:"isMeta"`
		TurnOrigin string `json:"turnOrigin"`
		APIError   bool   `json:"isApiErrorMessage"`
	}
	if json.Unmarshal(line, &v) != nil {
		return tailUseful
	}
	if v.UUID == "" {
		switch v.Type {
		case "api-request", "api-request-blob", "queue-operation":
			return tailNoise
		}
		return tailState
	}
	switch {
	case v.Type == "assistant" && v.APIError:
		return tailFailed
	case v.Type == "system" && failedTurnSystem[v.Subtype]:
		return tailNoise
	case v.Type == "user" && v.IsMeta && v.TurnOrigin == "scheduled":
		return tailNoise
	}
	return tailUseful
}

// failedTail returns where the failed end of a transcript of size bytes
// begins, and how many API errors it holds; size and 0 when there is none.
// The end kept runs to the last line of the conversation and the session state
// written right after it; what follows holds only API errors, request records
// and the scheduled prompts that got nothing else.
func failedTail(f io.ReaderAt, size int64) (int64, int, error) {
	for window := failedTailWindow; ; window = min(window*4, failedTailMax) {
		start := max(0, size-window)
		buf := make([]byte, size-start)
		if n, err := f.ReadAt(buf, start); n < len(buf) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return 0, 0, err
		}
		if !bytes.Contains(buf, apiErrorMark) {
			return size, 0, nil
		}
		cut, failed, found := scanFailedTail(buf, start > 0)
		if found {
			if failed == 0 {
				return size, 0, nil
			}
			return start + cut, failed, nil
		}
		if start == 0 || window >= failedTailMax {
			return size, 0, nil
		}
	}
}

// scanFailedTail reads buf (the end of a transcript, its first line partial
// when partial) backwards to the last line of the conversation. found is false
// when buf holds none.
func scanFailedTail(buf []byte, partial bool) (cut int64, failed int, found bool) {
	type seen struct {
		kind tailKind
		end  int
	}
	var after []seen // lines after the current one, last first
	for end := len(buf); end > 0; {
		start := bytes.LastIndexByte(buf[:end-1], '\n') + 1
		if start == 0 && partial {
			return 0, 0, false
		}
		k := classifyTail(bytes.TrimRight(buf[start:end], "\r\n"))
		if k == tailUseful {
			c := end
			for i := len(after) - 1; i >= 0 && after[i].kind == tailState; i-- {
				c = after[i].end
			}
			return int64(c), failed, true
		}
		if k == tailFailed {
			failed++
		}
		after = append(after, seen{k, end})
		end = start
	}
	return 0, 0, false
}
