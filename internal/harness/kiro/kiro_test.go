package kiro_test

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/harness/kiro"
)

func open(t *testing.T, source string) harness.Harness {
	t.Helper()
	h, err := kiro.New(config.HarnessSettings{Source: source, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func readFixture(t *testing.T, name string) []events.Event {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	evs, err := open(t, t.TempDir()).ReadSession(f, name, "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// summary is kind/tool-or-skill/ok:text per event, for compact comparison.
func summary(evs []events.Event) []string {
	var out []string
	for _, e := range evs {
		s := string(e.Kind)
		if e.Tool != "" {
			s += "/" + e.Tool
		}
		if e.Skill != "" {
			s += "/" + e.Skill
		}
		if e.OK != nil && !*e.OK {
			s += "/failed"
		}
		if e.Text != "" && e.Kind != events.ToolResult {
			s += ":" + e.Text
		}
		out = append(out, s)
	}
	return out
}

func TestV2Events(t *testing.T) {
	evs := readFixture(t, "v2")
	want := []string{
		"user_prompt:list the files in example.com's repo",
		"assistant_text:Listing them.",
		"tool_call/shell",
		"tool_call/read",
		"tool_result/shell",
		"tool_result/read/failed",
		"user_prompt:use ls -la instead",
	}
	if got := summary(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("events =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The prompt's time carries to the turns after it, which have none.
	for _, e := range evs {
		if e.Timestamp != "2026-10-18T11:08:20Z" || e.Harness != "kiro" || e.Host != "laptop-a" || e.Session != "v2" {
			t.Errorf("event metadata = %+v", e)
		}
	}
	if evs[1].Model != "claude-sonnet-5.5" {
		t.Errorf("model = %q", evs[1].Model)
	}
	if !strings.Contains(evs[4].Text, "README.md") {
		t.Errorf("tool result text = %q", evs[4].Text)
	}
}

func TestV3Events(t *testing.T) {
	evs := readFixture(t, "v3")
	want := []string{
		"user_prompt:why is the build failing?",
		"assistant_text:Let me check.",
		"tool_call/execute_bash",
		"tool_result/execute_bash/failed",
		"skill_invoked/load_resource/keep",
		"tool_result/load_resource",
		"user_prompt:stop and explain",
		"refusal",
	}
	if got := summary(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("events =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if evs[0].Timestamp != "2026-11-01T10:00:02.000Z" || evs[1].Model != "claude-opus-5.5" {
		t.Errorf("first events = %+v", evs[:2])
	}
}

func TestSessionID(t *testing.T) {
	h := open(t, t.TempDir())
	cases := map[string]string{
		"sessions/cli/abc.jsonl":                 "abc",
		"/home/you/.kiro/sessions/cli/abc.jsonl": "abc",
		"sessions/0f0f/sess_xyz/messages.jsonl":  "sess_xyz",
		"sessions/cli/abc.json":                  "",
		"sessions/cli/abc/tasks/t.jsonl":         "",
		"sessions/0f0f/sess_xyz/session.json":    "",
		"session-index/0f0f.jsonl":               "",
	}
	for p, want := range cases {
		got, ok := h.SessionID(p)
		if got != want || ok != (want != "") {
			t.Errorf("SessionID(%q) = %q, %v; want %q", p, got, ok, want)
		}
	}
}

func put(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOnDisk(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".kiro")
	src := filepath.Join(root, "sessions")
	v2, _ := os.ReadFile(filepath.Join("testdata", "v2.jsonl"))
	v3, _ := os.ReadFile(filepath.Join("testdata", "v3.jsonl"))
	put(t, filepath.Join(src, "cli", "abc.jsonl"), string(v2))
	put(t, filepath.Join(src, "cli", "abc.json"), `{"cwd":"/work/a"}`)
	put(t, filepath.Join(src, "0f0f", "sess_xyz", "messages.jsonl"), string(v3))
	put(t, filepath.Join(src, "0f0f", "sess_xyz", "session.json"), `{"workspacePaths":["/work/b"]}`)
	put(t, filepath.Join(src, "0f0f", ".index", ".lock"), "")
	put(t, filepath.Join(root, "steering", "AGENTS.md"), "- a rule\n")
	put(t, filepath.Join(root, "steering", "team", "build.md"), "- another\n")
	put(t, filepath.Join(root, "steering", "notes.txt"), "")
	h := open(t, src)

	sessions, err := h.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(src, "0f0f", "sess_xyz", "messages.jsonl"), filepath.Join(src, "cli", "abc.jsonl")}
	if !reflect.DeepEqual(sessions, want) {
		t.Errorf("sessions = %v", sessions)
	}
	for i, cwd := range []string{"/work/b", "/work/a"} {
		evs, err := h.ReadSessionFile(sessions[i], "laptop-a")
		if err != nil || len(evs) == 0 || evs[0].CWD != cwd {
			t.Errorf("%s: cwd = %v, %v", sessions[i], evs, err)
		}
	}
	ctx := h.ContextFiles()
	if !reflect.DeepEqual(ctx, []string{filepath.Join(root, "steering", "AGENTS.md"), filepath.Join(root, "steering", "team", "build.md")}) {
		t.Errorf("context files = %v", ctx)
	}
	files, err := h.WeekFiles(time.Time{}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, ".lock") {
			t.Errorf("week files include a lock: %v", files)
		}
	}
	if len(files) != 4 {
		t.Errorf("week files = %v", files)
	}
}

// An archived Kiro session recovers its working directory from the metadata
// file archived beside the transcript, in both layouts and whichever order
// the tar holds them; a session archived without metadata quietly keeps an
// empty one.
func TestReadTarZstCWD(t *testing.T) {
	v2, _ := os.ReadFile(filepath.Join("testdata", "v2.jsonl"))
	v3, _ := os.ReadFile(filepath.Join("testdata", "v3.jsonl"))
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	// Sorted archive order puts v3 metadata after its transcript and V2
	// metadata before, so both orders are covered.
	for _, f := range []struct{ name, body string }{
		{"sessions/0f0f/sess_bare/messages.jsonl", string(v3)},
		{"sessions/0f0f/sess_meta/messages.jsonl", string(v3)},
		{"sessions/0f0f/sess_meta/session.json", `{"workspacePaths":["/work/b"]}`},
		{"sessions/cli/abc.json", `{"cwd":"/work/a"}`},
		{"sessions/cli/abc.jsonl", string(v2)},
	} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	zw.Close()
	evs, err := harness.ReadTarZstFrom(open(t, t.TempDir()), &buf, "laptop-b")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"sess_meta": "/work/b", "sess_bare": "", "abc": "/work/a"}
	got := map[string]string{}
	for _, e := range evs {
		if cwd, ok := got[e.Session]; ok && cwd != e.CWD {
			t.Errorf("session %s has mixed cwds %q and %q", e.Session, cwd, e.CWD)
		}
		got[e.Session] = e.CWD
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cwds = %v, want %v", got, want)
	}
}

// An archived week names v3 sessions by their directory, not messages.jsonl.
func TestReadTarZst(t *testing.T) {
	v3, _ := os.ReadFile(filepath.Join("testdata", "v3.jsonl"))
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range map[string][]byte{
		"sessions/0f0f/sess_one/messages.jsonl": v3,
		"sessions/0f0f/sess_one/session.json":   []byte(`{}`),
		"sessions/0f0f/sess_two/messages.jsonl": v3,
	} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))})
		tw.Write(body)
	}
	tw.Close()
	zw.Close()
	evs, err := harness.ReadTarZstFrom(open(t, t.TempDir()), &buf, "laptop-b")
	if err != nil {
		t.Fatal(err)
	}
	sessions := map[string]int{}
	for _, e := range evs {
		sessions[e.Session]++
	}
	if len(sessions) != 2 || sessions["sess_one"] == 0 || sessions["sess_two"] == 0 {
		t.Errorf("sessions = %v", sessions)
	}
}
