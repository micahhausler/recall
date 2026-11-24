package v1

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The JSON here is the contract. A change to this test is a change to
// every plugin already written.
func TestWireFormat(t *testing.T) {
	const line = `{"harness":"chat","host":"laptop-a","session":"s-0001","cwd":"/home/you/src/example",` +
		`"repo":"example.com/you/example","timestamp":"2026-03-02T12:00:06Z","message_id":"a-1","parent_id":"u-1",` +
		`"kind":"tool_result","role":"assistant","text":"--- FAIL: TestParse","tool":"shell","input":"go test ./...",` +
		`"ok":false,"skill":"s","skill_dir":"/d","model":"m","sidechain":true}`
	e := Event{Harness: "chat", Host: "laptop-a", Session: "s-0001", CWD: "/home/you/src/example",
		Repo: "example.com/you/example", Timestamp: "2026-03-02T12:00:06Z", MessageID: "a-1", ParentID: "u-1",
		Kind: ToolResult, Role: "assistant", Text: "--- FAIL: TestParse", Tool: "shell", Input: "go test ./...",
		OK: Bool(false), Skill: "s", SkillDir: "/d", Model: "m", Sidechain: true}
	got, err := json.Marshal(e)
	if err != nil || string(got) != line {
		t.Errorf("event = %s, %v\nwant    %s", got, err, line)
	}
	var back Event
	if err := json.Unmarshal([]byte(line), &back); err != nil || back.OK == nil || *back.OK || back.ParentID != "u-1" {
		t.Errorf("decode = %+v, %v", back, err)
	}
	got, _ = json.Marshal(Event{Harness: "chat", Session: "s", Kind: UserPrompt})
	if want := `{"harness":"chat","session":"s","kind":"user_prompt"}`; string(got) != want {
		t.Errorf("empty fields = %s, want %s", got, want)
	}
}

func TestCheck(t *testing.T) {
	ok := Event{Harness: "chat", Session: "s", Timestamp: "2026-03-02T12:00:00Z", Kind: UserPrompt, Role: "user"}
	for _, k := range Kinds {
		e := ok
		e.Kind = k
		if err := Check(e); err != nil {
			t.Errorf("Check(%s) = %v", k, err)
		}
	}
	result := ok
	result.Kind, result.OK = ToolResult, Bool(false)
	if err := Check(result); err != nil {
		t.Errorf("Check(tool_result with ok) = %v", err)
	}
	child := ok
	child.MessageID, child.ParentID = "b", "a"
	if err := Check(child); err != nil {
		t.Errorf("Check(child) = %v", err)
	}

	bad := map[string]func(*Event){
		"timestamp":         func(e *Event) { e.Timestamp = "March 2nd" },
		"role":              func(e *Event) { e.Role = "system" },
		"ok on a call":      func(e *Event) { e.Kind, e.OK = ToolCall, Bool(true) },
		"long message id":   func(e *Event) { e.MessageID = strings.Repeat("m", MaxIDLen+1) },
		"long parent id":    func(e *Event) { e.MessageID, e.ParentID = "m", strings.Repeat("p", MaxIDLen+1) },
		"parent without id": func(e *Event) { e.ParentID = "a" },
		"own parent":        func(e *Event) { e.MessageID, e.ParentID = "a", "a" },
	}
	for name, mutate := range bad {
		e := ok
		mutate(&e)
		if err := Check(e); err == nil {
			t.Errorf("Check accepts a bad %s", name)
		} else if errors.Is(err, ErrUnknownKind) {
			t.Errorf("Check(%s) = %v, not an unknown kind", name, err)
		}
	}
	unknown := ok
	unknown.Kind = "hologram"
	if err := Check(unknown); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("Check(unknown kind) = %v, want ErrUnknownKind", err)
	}
}

func TestCheckSession(t *testing.T) {
	ev := func(msg, parent, ts string) Event {
		return Event{Harness: "chat", Session: "s", Timestamp: ts, MessageID: msg, ParentID: parent, Kind: AssistantText}
	}
	const ts = "2026-03-02T12:00:00Z"
	good := []Event{ev("u1", "", ts), ev("a1", "u1", ""), ev("u2", "", ts), ev("a2", "u2", "")}
	if err := CheckSession(good); err != nil {
		t.Errorf("CheckSession(tree) = %v", err)
	}
	if err := CheckSession([]Event{ev("", "", ts), ev("", "", "")}); err != nil {
		t.Errorf("CheckSession(flat) = %v", err)
	}
	other := ev("", "", ts)
	other.Session = "t"
	cases := map[string][]Event{
		"a parent named later":   {ev("a1", "u1", ts), ev("u1", "", ts)},
		"a parent never named":   {ev("u1", "", ts), ev("a1", "missing", ts)},
		"no dated event":         {ev("u1", "", ""), ev("a1", "u1", "")},
		"two sessions":           {ev("", "", ts), other},
		"no events":              nil,
		"a bad event":            {ev("u1", "", "yesterday")},
		"an unknown kind inside": {{Harness: "chat", Session: "s", Timestamp: ts, Kind: "hologram"}},
	}
	for name, evs := range cases {
		if err := CheckSession(evs); err == nil {
			t.Errorf("CheckSession accepts %s", name)
		}
	}
}

func TestSummarize(t *testing.T) {
	cases := []struct {
		tool, input, want string
	}{
		{"Bash", `{"command":"go test\n  ./..."}`, "go test ./..."},
		{"Read", `{"file_path":"/home/you/a.go"}`, "/home/you/a.go"},
		{"NotebookEdit", `{"notebook_path":"/home/you/n.ipynb"}`, "/home/you/n.ipynb"},
		{"Grep", `{"pattern":"TODO","path":"internal"}`, "TODO in internal"},
		{"Grep", `{"pattern":"TODO"}`, "TODO"},
		{"Glob", `{"pattern":"**/*.go"}`, "**/*.go"},
		{"Task", `{"description":"survey"}`, "survey"},
		{"WebFetch", `{"url":"https://example.com"}`, "https://example.com"},
		{"WebSearch", `{"query":"recall"}`, "recall"},
		{"mcp__s__t", `{"a":1}`, `{"a":1}`},
		{"mcp__s__t", `{}`, ""},
		{"Bash", `not json`, ""},
	}
	for _, c := range cases {
		if got := Summarize(c.tool, json.RawMessage(c.input)); got != c.want {
			t.Errorf("Summarize(%s, %s) = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
	long := strings.Repeat("é", InputLimit+5)
	if got := TrimInput(long); got != strings.Repeat("é", InputLimit)+"…" {
		t.Errorf("TrimInput cut by bytes, not characters: %d runes", len([]rune(got)))
	}
	if TrimInput("short") != "short" {
		t.Error("TrimInput changed a short input")
	}
}
