package sessions

import (
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The regular expressions the finders replace: the reference they must agree with.
var (
	refShortText  = regexp.MustCompile(`(?:with ID: |<task-id>|agentId: |tasks[\\/]+)([A-Za-z0-9]{6,24})(?:</task-id>|\.output|[^A-Za-z0-9]|$)`)
	refShortField = regexp.MustCompile(`"(?:agentId|agent_id|[A-Za-z]*[Tt]ask_?[Ii]d|task_id|bash_id|bashId|shell_id|shellId)"\s*:\s*"([A-Za-z0-9]{6,24})"`)
)

func refAll(re *regexp.Regexp, b []byte) []string {
	var out []string
	for _, m := range re.FindAllSubmatch(b, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

func finderAll(each func([]byte, func([]byte)), b []byte) []string {
	var out []string
	each(b, func(v []byte) { out = append(out, string(v)) })
	return out
}

var idFindCases = []string{
	``,
	`{"agentId":"a0123456789abcdef"}`,
	`{"taskId":"b01bxagzb","task_id":"bk7f3m2q1","Task_Id":"abc123","backgroundTaskId":"xyz789q","taskid":"q1w2e3"}`,
	`{"bash_id":"abc123","bashId":"abc124","shell_id":"abc125","shellId":"abc126","subtaskId":"abc127"}`,
	`{"my_taskId":"abc123","taskID":"abc123","task-id":"abc123","1taskId":"abc123","sessionId":"abc123"}`,
	`{"taskId" : "abc123", "taskId":	"abc124","taskId":"abc12","taskId":"` + strings.Repeat("a", 25) + `"}`,
	`{"taskId":"abc-123","taskId":"abc123x!","agentId":"","agentId":"a1b2c3"`,
	`"x""taskId":"abc123""taskId":"abc124"`,
	`\"taskId\":\"abc123\"`,
	`Started with ID: b01bxagzb. Output at tasks/b01bxagzb.output and <task-id>bqwertyui</task-id>`,
	`agentId: a0123456789abcdef (use it) agentId: short agentId: ` + strings.Repeat("z", 30),
	`C:\\Users\\x\\tasks\\\\bk7f3m2q1.output and tasks//abc123 and tasks abc123 and subtasks/abcdef9`,
	`with ID: abcdtasks/b12345678 then with ID: b123456789012345678901234 end`,
	`<task-id>b0000001</task-id><task-id>b0000002</task-id>with ID: b0000003`,
	`with ID: b0000004`,
	`tasks/`,
	`{"slug":"calm-blue-river","slug" : "a","slug":"-x","slug":"x_y-z9","slug":"` + strings.Repeat("s", 81) + `","slug":"` + strings.Repeat("s", 82) + `"}`,
	`"slug":"ab""slug":"cd"`,
	`"slug":"bad slug","slug":"ok-1"`,
}

func TestFindersMatchTheExpressions(t *testing.T) {
	check := func(s string) {
		b := []byte(s)
		if got, want := finderAll(eachShortField, b), refAll(refShortField, b); !reflect.DeepEqual(got, want) {
			t.Errorf("fields of %q: got %q, want %q", s, got, want)
		}
		if got, want := finderAll(eachShortText, b), refAll(refShortText, b); !reflect.DeepEqual(got, want) {
			t.Errorf("text of %q: got %q, want %q", s, got, want)
		}
		if got, want := finderAll(eachSlug, b), refAll(slugField, b); !reflect.DeepEqual(got, want) {
			t.Errorf("slugs of %q: got %q, want %q", s, got, want)
		}
	}
	for _, s := range idFindCases {
		check(s)
	}
	pieces := []string{
		`"`, `":"`, `" : "`, `:`, ` `, "\t", `\`, `\\`, `/`, `-`, `_`, `!`, `.`, `<`, `>`,
		`agentId`, `agent_id`, `taskId`, `Task_Id`, `taskid`, `bgTaskId`, `bashId`, `shell_id`, `sessionId`, `uuid`, `taskID`,
		`with ID: `, `<task-id>`, `</task-id>`, `agentId: `, `tasks`, `tasks/`, `.output`, `slug`, `"slug"`,
		`abc123`, `b01bxagzb`, `a0123456789abcdef`, `xyz`, `q`, `12345`, strings.Repeat("k", 24), strings.Repeat("k9", 13),
		`calm-blue-river`, `d"`, `id"`,
	}
	r := rand.New(rand.NewSource(7))
	for range 30000 {
		var b strings.Builder
		for range r.Intn(14) {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		check(b.String())
	}
}

func TestIsShortKey(t *testing.T) {
	for k, want := range map[string]bool{
		"agentId": true, "agent_id": true, "taskId": true, "TaskId": true, "task_id": true, "Task_Id": true, "taskid": true,
		"backgroundTaskId": true, "subtaskId": true, "bash_id": true, "bashId": true, "shell_id": true, "shellId": true,
		"taskID": false, "my_taskId": false, "1taskId": false, "askId": false, "task__id": false, "sessionId": false, "Id": false, "": false,
	} {
		if got := isShortKey([]byte(k)); got != want {
			t.Errorf("%q: %v, want %v", k, got, want)
		}
		re := regexp.MustCompile(`^(?:agentId|agent_id|[A-Za-z]*[Tt]ask_?[Ii]d|task_id|bash_id|bashId|shell_id|shellId)$`)
		if re.MatchString(k) != want {
			t.Errorf("%q: the expression says %v", k, re.MatchString(k))
		}
	}
}

// line, writing its steps into two buffers it reuses, gives what the steps
// give one after the other, and a line it returned stays as it was.
func TestLineWithScratchMatchesTheSteps(t *testing.T) {
	m := newIDSet(forkID(sid))
	m.old = sid
	for _, u := range []string{"aaaaaaaa-0000-4000-8000-000000000001", "aaaaaaaa-0000-4000-8000-000000000002"} {
		m.uuids[u] = true
	}
	for _, id := range []string{"a0123456789abcdef", "bk7f3m2q1"} {
		m.add(id)
	}
	m.slugs["calm-blue-river"] = "calm-blue-river-" + m.fid[:8]
	m.prepare()
	steps := func(b []byte) []byte {
		b = []byte(strings.ReplaceAll(string(b), m.old, m.fid))
		b = m.mapUUIDs(b)
		b = m.mapAPIIDs(b)
		b = m.shortIDs(b)
		for old, as := range m.slugs {
			b = replaceWord(b, old, as)
		}
		return b
	}
	pieces := []string{sid, "aaaaaaaa-0000-4000-8000-000000000001", "AAAAAAAA-0000-4000-8000-000000000002", "aaaaaaaa-0000-4000-8000-00000000000f",
		"toolu_01ABCDEFGHIJKLMNOPQRSTUVW", "call_f2bafbdac980443f9b296d95129b00b6", "a0123456789abcdef", "bk7f3m2q1", "calm-blue-river",
		"calm-blue-rivers", " ", `"`, `\n`, "x", strings.Repeat("y", 3000)}
	r := rand.New(rand.NewSource(9))
	var got, want []string
	for range 3000 {
		var b strings.Builder
		for range r.Intn(12) {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		got = append(got, string(m.line([]byte(b.String()))))
		want = append(want, string(steps([]byte(b.String()))))
	}
	kept := make([][]byte, 0, 50)
	for i := range 50 { // returned slices must not share the reused buffers
		kept = append(kept, m.line([]byte(sid+" "+strings.Repeat("z", i))))
	}
	for i, b := range kept {
		if string(b) != m.fid+" "+strings.Repeat("z", i) {
			t.Fatalf("line %d changed after later calls: %q", i, b)
		}
	}
	if !reflect.DeepEqual(got, want) {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("line %d: got %q, want %q", i, got[i], want[i])
			}
		}
	}
}
