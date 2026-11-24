package harness_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/archive"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/harness"
)

const storedSession = `{"harness":"other","host":"laptop-z","session":"wrong","cwd":"/home/you/src/example","timestamp":"2026-03-02T12:00:00Z","kind":"user_prompt","role":"user","text":"run the tests","extra":"dropped"}
{"harness":"other","host":"laptop-z","session":"wrong","timestamp":"2026-03-02T12:00:04Z","kind":"tool_call","role":"assistant","tool":"shell","input":"go test ./..."}
{"harness":"other","session":"wrong","timestamp":"2026-03-02T12:00:05Z","kind":"hologram"}
not json
{"harness":"other","session":"wrong","timestamp":"2026-03-02T12:00:06Z","kind":"tool_result","role":"assistant","tool":"shell","input":"go test ./...","ok":false,"text":"--- FAIL"}
`

func eventsHarness(t *testing.T, source string) harness.Harness {
	t.Helper()
	h, err := harness.Open("chat", config.HarnessSettings{Source: source, Enabled: true, Format: config.FormatEvents})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestEventsReadSession(t *testing.T) {
	h := eventsHarness(t, t.TempDir())
	if h.Name() != "chat" {
		t.Errorf("name = %q", h.Name())
	}
	evs, err := h.ReadSession(strings.NewReader(storedSession), "s-0001", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3 with the unknown kind and the torn line dropped: %+v", len(evs), evs)
	}
	for _, e := range evs {
		if e.Harness != "chat" || e.Host != "laptop-a" || e.Session != "s-0001" {
			t.Errorf("harness, host, session = %q, %q, %q; recall writes its own", e.Harness, e.Host, e.Session)
		}
	}
	if evs[0].Text != "run the tests" || evs[0].CWD != "/home/you/src/example" || evs[2].OK == nil || *evs[2].OK {
		t.Errorf("events = %+v", evs)
	}
}

func TestEventsSessions(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, "sessions")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"s-0001.jsonl":        storedSession,
		"notes.txt":           "not a session",
		"nested/s-0002.jsonl": storedSession,
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(src, "s-0001.jsonl"), filepath.Join(src, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	h := eventsHarness(t, src)
	got, err := h.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(src, "s-0001.jsonl")}; !reflect.DeepEqual(got, want) {
		t.Errorf("sessions = %v, want %v", got, want)
	}
	for path, want := range map[string]bool{
		"sessions/s-0001.jsonl":        true,
		"sessions/nested/s-0002.jsonl": false,
		"run-1/s-0001.jsonl":           false,
		"sessions/notes.txt":           false,
	} {
		if _, ok := h.SessionID(path); ok != want {
			t.Errorf("SessionID(%q) ok = %v, want %v", path, ok, want)
		}
	}
	evs, err := h.ReadSessionFile(got[0], "laptop-a")
	if err != nil || len(evs) != 3 || evs[0].Session != "s-0001" {
		t.Errorf("ReadSessionFile = %d events, %v", len(evs), err)
	}

	empty := eventsHarness(t, filepath.Join(home, "missing"))
	if s, err := empty.Sessions(); err != nil || len(s) != 0 {
		t.Errorf("missing source = %v, %v", s, err)
	}
	inv, err := h.Inventory(t.Context(), home)
	if err != nil || len(inv.Skills)+len(inv.Commands) != 0 || len(h.ContextFiles()) != 0 {
		t.Errorf("inventory %+v, %v; context files %v", inv, err, h.ContextFiles())
	}
	if mems, err := h.Memories(); err != nil || len(mems) != 0 {
		t.Errorf("memories = %v, %v", mems, err)
	}
}

// A stored session archives the way the weekly archive builds it and
// reads back under the harness's name.
func TestEventsArchiveRoundTrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(src, "s-0001.jsonl")
	if err := os.WriteFile(path, []byte(storedSession), 0o644); err != nil {
		t.Fatal(err)
	}
	h := eventsHarness(t, src)
	now := time.Now()
	files, err := h.WeekFiles(now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(files, []string{"sessions/s-0001.jsonl"}) {
		t.Fatalf("week files = %v, %v", files, err)
	}
	var buf bytes.Buffer
	if _, err := archive.Build(filepath.Dir(src), files, &buf, archive.Options{}); err != nil {
		t.Fatal(err)
	}
	got, err := harness.ReadTarZstFrom(h, &buf, "")
	if err != nil {
		t.Fatal(err)
	}
	want, err := h.ReadSessionFile(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || len(got) != 3 {
		t.Errorf("archive read = %+v\nwant %+v", got, want)
	}
}
