package harness_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/archive"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/harness"
)

func claude(t *testing.T, source string) harness.Harness {
	t.Helper()
	h, err := harness.Open("claude", config.HarnessSettings{Source: source, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func readFixture(t *testing.T, name string) []events.Event {
	t.Helper()
	h := claude(t, t.TempDir())
	f, err := os.Open(filepath.Join("testdata", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	evs, err := h.ReadSession(f, name, "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func kinds(evs []events.Event) []events.Kind {
	var out []events.Kind
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func kindsEqual(a []events.Kind, b ...events.Kind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPlainFixture(t *testing.T) {
	evs := readFixture(t, "plain")
	if !kindsEqual(kinds(evs), events.UserPrompt, events.ToolResult, events.UserPrompt, events.SlashCommand, events.UserPrompt) {
		t.Fatalf("kinds = %v", kinds(evs))
	}
	one := evs[0]
	if one.Text != "one" || one.Harness != "claude" || one.Host != "laptop-a" ||
		one.Session != "plain" || one.CWD != "/w" || one.Sidechain {
		t.Errorf("first prompt = %+v", one)
	}
	if res := evs[1]; res.OK == nil || !*res.OK || res.Text != "x" {
		t.Errorf("tool result = %+v", res)
	}
	if sub := evs[2]; !sub.Sidechain || sub.Text != "subagent" {
		t.Errorf("sidechain prompt = %+v", sub)
	}
	if cmd := evs[3]; cmd.Skill != "clear" {
		t.Errorf("slash command = %+v", cmd)
	}
	if two := evs[4]; two.Text != "two" {
		t.Errorf("last prompt = %+v", two)
	}
}

func TestCompactFixture(t *testing.T) {
	evs := readFixture(t, "compact")
	if !kindsEqual(kinds(evs), events.UserPrompt, events.Compact, events.UserPrompt) {
		t.Fatalf("kinds = %v", kinds(evs))
	}
	if !strings.Contains(evs[1].Text, "continued from a previous conversation") {
		t.Errorf("compact text = %q", evs[1].Text)
	}
}

func TestSdkFixtureHasNoTypedPrompts(t *testing.T) {
	if evs := readFixture(t, "sdk"); len(evs) != 0 {
		t.Errorf("events = %+v, want none for a sdk-cli session", evs)
	}
}

func TestGatedFixturesStillEmitEvents(t *testing.T) {
	// Dropping gated chats is distill policy; the event stream stays faithful.
	evs := readFixture(t, "gated")
	if !kindsEqual(kinds(evs), events.UserPrompt) {
		t.Fatalf("kinds = %v", kinds(evs))
	}
	if evs := readFixture(t, "gated-usage"); len(evs) != 1 {
		t.Errorf("gated-usage events = %+v", evs)
	}
}

func TestCyberFixtureRefusals(t *testing.T) {
	evs := readFixture(t, "cyber")
	want := []events.Kind{
		events.UserPrompt, events.UserPrompt, events.UserPrompt, events.UserPrompt, events.UserPrompt,
		events.Refusal, events.Refusal,
		events.UserPrompt,
	}
	if !kindsEqual(kinds(evs), want...) {
		t.Fatalf("kinds = %v", kinds(evs))
	}
	if evs[5].Text != "cyber" || evs[5].Model != "claude-opus-5-5" {
		t.Errorf("refusal = %+v", evs[5])
	}
}

const toolSession = `{"type":"user","timestamp":"2026-11-01T00:00:01Z","cwd":"/w","entrypoint":"cli","message":{"role":"user","content":"run the tests"}}
{"type":"assistant","timestamp":"2026-11-01T00:00:02Z","requestedModel":"claude-opus-5-5","message":{"model":"m","stop_reason":null,"content":[{"type":"text","text":"Running them."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test"}}]}}
{"type":"user","timestamp":"2026-11-01T00:00:03Z","cwd":"/w","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":[{"type":"text","text":"FAIL"}]}]}}
{"type":"assistant","timestamp":"2026-11-01T00:00:04Z","requestedModel":"claude-opus-5-5","message":{"model":"m","stop_reason":null,"content":[{"type":"tool_use","id":"t2","name":"Skill","input":{"skill":"fix-tests"}}]}}
{"type":"user","timestamp":"2026-11-01T00:00:05Z","cwd":"/w","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"done"}]}}
`

func TestToolCallsResultsAndSkills(t *testing.T) {
	h := claude(t, t.TempDir())
	evs, err := h.ReadSession(strings.NewReader(toolSession), "s", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	if !kindsEqual(kinds(evs), events.UserPrompt, events.AssistantText, events.ToolCall,
		events.ToolResult, events.SkillInvoked, events.ToolResult) {
		t.Fatalf("kinds = %v", kinds(evs))
	}
	if call := evs[2]; call.Tool != "Bash" || call.Model != "claude-opus-5-5" {
		t.Errorf("tool call = %+v", call)
	}
	if res := evs[3]; res.Tool != "Bash" || res.OK == nil || *res.OK || res.Text != "FAIL" {
		t.Errorf("failed tool result = %+v; is_error must mean ok=false", res)
	}
	if sk := evs[4]; sk.Skill != "fix-tests" || sk.Tool != "Skill" {
		t.Errorf("skill invocation = %+v", sk)
	}
	if res := evs[5]; res.Tool != "Skill" || res.OK == nil || !*res.OK {
		t.Errorf("skill result = %+v", res)
	}
}

func TestTornLastLineIsSkipped(t *testing.T) {
	h := claude(t, t.TempDir())
	evs, err := h.ReadSession(strings.NewReader(
		`{"type":"user","timestamp":"2026-11-01T00:00:01Z","entrypoint":"cli","message":{"content":"hello"}}`+"\n"+
			`{"type":"user","timestamp":"2026-11-01T00:0`), "s", "")
	if err != nil || len(evs) != 1 {
		t.Errorf("events = %+v, %v; want the torn line skipped", evs, err)
	}
}

func TestSessionsAndArchiveReading(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, ".claude", "projects")
	if err := os.MkdirAll(filepath.Join(source, "-home-you"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("testdata", "plain.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(source, "-home-you", "11111111-2222-3333-4444-555555555555.jsonl")
	if err := os.WriteFile(session, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "-home-you", "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := claude(t, source)

	sessions, err := h.Sessions()
	if err != nil || len(sessions) != 1 || !strings.HasSuffix(sessions[0], ".jsonl") {
		t.Fatalf("sessions = %v, %v", sessions, err)
	}
	evs, err := h.ReadSessionFile(sessions[0], "laptop-a")
	if err != nil || len(evs) != 5 {
		t.Fatalf("events = %d, %v", len(evs), err)
	}
	if evs[0].Session != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("session id = %s", evs[0].Session)
	}

	// The same session read from inside a week's archive gives the same events.
	now := time.Now()
	os.Chtimes(session, now, now)
	tarPath := filepath.Join(root, "week.tar.zst")
	files := []string{"projects/-home-you/11111111-2222-3333-4444-555555555555.jsonl"}
	if _, _, err := archive.BuildFile(filepath.Dir(source), files, tarPath); err != nil {
		t.Fatal(err)
	}
	fromTar, err := harness.ReadTarZst(h, tarPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fromTar) != len(evs) {
		t.Fatalf("archive events = %d, disk events = %d", len(fromTar), len(evs))
	}
	for i := range evs {
		want := evs[i]
		want.Host = ""
		wj, _ := json.Marshal(want)
		gj, _ := json.Marshal(fromTar[i])
		if string(wj) != string(gj) {
			t.Errorf("event %d differs: disk %s, archive %s", i, wj, gj)
		}
	}
}
