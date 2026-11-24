package imports

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	importsv1 "github.com/micahhausler/recall/api/imports/v1"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/harness"
)

var start = time.Date(2026, 3, 9, 6, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func options(t *testing.T) Options {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "import", "chat")
	return Options{Name: "chat", Host: "laptop-a", Dir: dir, Sessions: filepath.Join(dir, "sessions"),
		Now: start, LockStale: time.Hour}
}

func stored(t *testing.T, o Options, id string) []events.Event {
	t.Helper()
	h, err := harness.Open(o.Name, config.HarnessSettings{Source: o.Sessions, Enabled: true, Format: config.FormatEvents})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := h.ReadSessionFile(filepath.Join(o.Sessions, id+".jsonl"), o.Host)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestRunValidatesEveryFile(t *testing.T) {
	o := options(t)
	files := map[string]string{}
	for _, name := range []string{"valid", "combined", "unknown-kind", "undated", "dotdot", "slash",
		"dup-a", "dup-b", "orphan", "bad-role", "malformed", "long-line"} {
		files[name+".jsonl"] = fixture(t, name+".jsonl")
	}
	files["notes.txt"] = "not a session"
	src := &Fake{Files: files, Status: importsv1.Status{Sessions: 12, Filtered: 2}, Prepare: func(dir string) error {
		if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link.jsonl")); err != nil {
			return err
		}
		if err := syscall.Mkfifo(filepath.Join(dir, "fifo.jsonl"), 0o600); err != nil {
			return err
		}
		return os.Mkdir(filepath.Join(dir, "sub.jsonl"), 0o700)
	}}
	o.Limits = importsv1.DefaultLimits
	o.Limits.LineBytes = 4096
	res, err := Run(context.Background(), src, o)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"chat-c-0001", "s-0001", "s-0003"}; !reflect.DeepEqual(res.Stored, want) {
		t.Errorf("stored = %v, want %v", res.Stored, want)
	}
	rejected := map[string]string{}
	for _, r := range res.Rejected {
		rejected[r.File] = r.Reason
	}
	for file, reason := range map[string]string{
		"undated.jsonl":   "timestamp",
		"dotdot.jsonl":    "isn't 1 to 128",
		"slash.jsonl":     "isn't 1 to 128",
		"dup-a.jsonl":     "in 2 files",
		"dup-b.jsonl":     "in 2 files",
		"orphan.jsonl":    "names no earlier message",
		"bad-role.jsonl":  "role",
		"malformed.jsonl": "line 2 isn't a JSON event",
		"long-line.jsonl": "over the limit of 4096 bytes",
	} {
		if !strings.Contains(rejected[file], reason) {
			t.Errorf("%s rejected for %q, want %q", file, rejected[file], reason)
		}
	}
	if len(res.Rejected) != 9 {
		t.Errorf("rejected = %+v", res.Rejected)
	}
	sort.Strings(res.Skipped)
	if want := []string{"fifo.jsonl", "link.jsonl", "notes.txt", "sub.jsonl"}; !reflect.DeepEqual(res.Skipped, want) {
		t.Errorf("skipped = %v, want %v", res.Skipped, want)
	}
	if res.Found != 12 || res.Dropped != 1 || res.Status.Filtered != 2 || !res.SinceMoved {
		t.Errorf("result = %+v", res)
	}

	evs := stored(t, o, "s-0001")
	if len(evs) != 4 {
		t.Fatalf("s-0001 = %+v", evs)
	}
	for _, e := range evs {
		if e.Harness != "chat" || e.Host != "laptop-a" || e.Session != "s-0001" {
			t.Errorf("event %+v doesn't carry recall's own harness and host", e)
		}
	}
	data, _ := os.ReadFile(filepath.Join(o.Sessions, "s-0001.jsonl"))
	if strings.Contains(string(data), "unknown_field") || strings.Contains(string(data), "laptop-z") {
		t.Errorf("stored file keeps what the plugin wrote: %s", data)
	}
	tree := stored(t, o, "chat-c-0001")
	if tree[1].ParentID != "u-1" || tree[4].MessageID != "u-2" {
		t.Errorf("tree = %+v", tree)
	}
	if got := stored(t, o, "s-0003"); len(got) != 1 {
		t.Errorf("s-0003 = %+v, want the unknown kind dropped", got)
	}
	if st, err := os.Stat(o.Sessions); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("sessions dir = %v, %v", st, err)
	}
	if runs, _ := filepath.Glob(filepath.Join(o.Dir, "run-*")); len(runs) != 0 {
		t.Errorf("run directories left behind: %v", runs)
	}
	spec := src.Specs[0]
	if spec.Name != "chat" || spec.Since != "" || filepath.Dir(spec.OutputDir) != o.Dir || !strings.HasPrefix(filepath.Base(spec.OutputDir), "run-") {
		t.Errorf("spec = %+v", spec)
	}
}

// Text and input are redacted whatever archive-chats.redact says, and
// input is cut as every adapter cuts it.
func TestRunRedactsAndCuts(t *testing.T) {
	o := options(t)
	token := "gh" + "p_" + strings.Repeat("a1B2", 9)
	long := strings.Repeat("x", 300)
	body := `{"session":"s-1","timestamp":"2026-03-02T12:00:00Z","kind":"user_prompt","role":"user","text":"use ` + token + `"}` + "\n" +
		`{"session":"s-1","timestamp":"2026-03-02T12:00:01Z","kind":"tool_call","role":"assistant","tool":"shell","input":"curl -H ` + token + ` ` + long + `"}` + "\n"
	res, err := Run(context.Background(), &Fake{Files: map[string]string{"s.jsonl": body}, Status: importsv1.Status{Sessions: 1}}, o)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(o.Sessions, "s-1.jsonl"))
	if strings.Contains(string(data), token) || res.Redactions != 2 {
		t.Errorf("redactions = %d, stored %s", res.Redactions, data)
	}
	evs := stored(t, o, "s-1")
	if n := len([]rune(evs[1].Input)); n != 201 {
		t.Errorf("input is %d characters, want 200 and the cut mark", n)
	}
}

func TestRunStoresOnlyChanges(t *testing.T) {
	o := options(t)
	src := &Fake{Files: map[string]string{"x.jsonl": fixture(t, "valid.jsonl")}, Status: importsv1.Status{Sessions: 1}}
	if _, err := Run(context.Background(), src, o); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(o.Sessions, "s-0001.jsonl")
	old := start.Add(-24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	// The same session again changes nothing, not even its mtime.
	o.Now = start.Add(24 * time.Hour)
	res, err := Run(context.Background(), src, o)
	if err != nil || res.Unchanged != 1 || len(res.Stored) != 0 {
		t.Fatalf("second run = %+v, %v", res, err)
	}
	if st, _ := os.Stat(path); !st.ModTime().Equal(old) {
		t.Errorf("an unchanged session was rewritten: mtime %v", st.ModTime())
	}

	// A grown session replaces the stored one.
	grown := fixture(t, "valid.jsonl") + `{"session":"s-0001","timestamp":"2026-03-02T12:01:00Z","kind":"user_prompt","role":"user","text":"thanks"}` + "\n"
	src.Files["x.jsonl"] = grown
	if res, err := Run(context.Background(), src, o); err != nil || !reflect.DeepEqual(res.Stored, []string{"s-0001"}) {
		t.Fatalf("grown run = %+v, %v", res, err)
	}
	if evs := stored(t, o, "s-0001"); len(evs) != 5 {
		t.Errorf("stored %d events, want 5", len(evs))
	}

	// A shorter copy is held until Replace.
	src.Files["x.jsonl"] = fixture(t, "shorter.jsonl")
	res, err = Run(context.Background(), src, o)
	if err != nil || len(res.Stored) != 0 || !reflect.DeepEqual(res.Held, []Held{{ID: "s-0001", Drop: 3}}) {
		t.Fatalf("shorter run = %+v, %v", res, err)
	}
	if evs := stored(t, o, "s-0001"); len(evs) != 5 {
		t.Errorf("a held copy replaced the stored one: %d events", len(evs))
	}
	o.Replace = true
	if res, err := Run(context.Background(), src, o); err != nil || len(res.Stored) != 1 {
		t.Fatalf("replace run = %+v, %v", res, err)
	}
	if evs := stored(t, o, "s-0001"); len(evs) != 2 {
		t.Errorf("--replace kept %d events, want 2", len(evs))
	}
}

func TestRunSince(t *testing.T) {
	o := options(t)
	src := &Fake{Status: importsv1.Status{}}
	ctx := context.Background()
	if res, err := Run(ctx, src, o); err != nil || !res.SinceMoved {
		t.Fatalf("first run = %+v, %v", res, err)
	}
	if src.Specs[0].Since != "" {
		t.Errorf("first run asked from %q", src.Specs[0].Since)
	}
	first := o.Now
	o.Now = first.Add(24 * time.Hour)
	src.Status.Partial = true
	if res, err := Run(ctx, src, o); err != nil || res.SinceMoved {
		t.Fatalf("partial run = %+v, %v", res, err)
	}
	if want := first.Add(-time.Hour).Format(time.RFC3339); src.Specs[1].Since != want {
		t.Errorf("second run asked from %q, want an hour before the first run, %q", src.Specs[1].Since, want)
	}
	o.Now = first.Add(48 * time.Hour)
	src.Status.Partial = false
	Run(ctx, src, o)
	if src.Specs[2].Since != src.Specs[1].Since {
		t.Errorf("a partial run moved since: %q", src.Specs[2].Since)
	}
	o.Full = true
	Run(ctx, src, o)
	if src.Specs[3].Since != "" {
		t.Errorf("--full asked from %q", src.Specs[3].Since)
	}
	state, err := ReadState(o.Dir)
	if err != nil || !state.Since.Equal(o.Now) || state.Last == nil || !state.Last.OK {
		t.Errorf("state = %+v, %v", state, err)
	}
}

func TestRunFailureStoresNothing(t *testing.T) {
	o := options(t)
	ctx := context.Background()
	good := &Fake{Files: map[string]string{"x.jsonl": fixture(t, "valid.jsonl")}, Status: importsv1.Status{Sessions: 1}}
	if _, err := Run(ctx, good, o); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(o.Sessions, "s-0001.jsonl"))
	state, _ := ReadState(o.Dir)

	o.Now = start.Add(24 * time.Hour)
	bad := &Fake{Files: map[string]string{"x.jsonl": fixture(t, "shorter.jsonl"), "y.jsonl": fixture(t, "combined.jsonl")},
		Err: errors.New("login expired")}
	if _, err := Run(ctx, bad, o); err == nil || !strings.Contains(err.Error(), "login expired") {
		t.Fatalf("failed run = %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(o.Sessions, "s-0001.jsonl"))
	if string(after) != string(before) {
		t.Error("a failed run changed a stored session")
	}
	if _, err := os.Stat(filepath.Join(o.Sessions, "chat-c-0001.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a failed run stored a session")
	}
	now, _ := ReadState(o.Dir)
	if !now.Since.Equal(state.Since) || now.Last == nil || now.Last.OK || !strings.Contains(now.Last.Error, "login expired") {
		t.Errorf("state after failure = %+v", now)
	}
	if runs, _ := filepath.Glob(filepath.Join(o.Dir, "run-*")); len(runs) != 0 {
		t.Errorf("run directories left behind: %v", runs)
	}
}

func TestRunLimitsFailTheRun(t *testing.T) {
	ctx := context.Background()
	o := options(t)
	o.Limits = importsv1.DefaultLimits
	o.Limits.Sessions = 1
	two := &Fake{Files: map[string]string{"a.jsonl": fixture(t, "valid.jsonl"), "b.jsonl": fixture(t, "combined.jsonl")}}
	if _, err := Run(ctx, two, o); err == nil || !strings.Contains(err.Error(), "over the limit of 1") {
		t.Errorf("too many files = %v", err)
	}
	o.Limits = importsv1.DefaultLimits
	o.Limits.TotalBytes = 100
	if _, err := Run(ctx, two, o); err == nil || !strings.Contains(err.Error(), "nothing was stored") {
		t.Errorf("too many bytes = %v", err)
	}
	if entries, _ := os.ReadDir(o.Sessions); len(entries) != 0 {
		t.Errorf("a failed run stored %d sessions", len(entries))
	}
	o.Limits = importsv1.DefaultLimits
	o.Limits.SessionBytes = 100
	res, err := Run(ctx, two, o)
	if err != nil || len(res.Rejected) != 2 || !strings.Contains(res.Rejected[0].Reason, "over the limit of 100") {
		t.Errorf("an oversize session = %+v, %v", res, err)
	}
}

func TestRunDryRun(t *testing.T) {
	o := options(t)
	o.DryRun = true
	res, err := Run(context.Background(), &Fake{Files: map[string]string{"x.jsonl": fixture(t, "valid.jsonl")}, Status: importsv1.Status{Sessions: 1}}, o)
	if err != nil || !reflect.DeepEqual(res.Stored, []string{"s-0001"}) || res.SinceMoved {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(o.Sessions, "s-0001.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a dry run stored a session")
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a dry run wrote state")
	}
}

func TestRunLockAndLeftovers(t *testing.T) {
	o := options(t)
	if err := os.MkdirAll(filepath.Join(o.Dir, "run-crashed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), &Fake{}, o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "run-crashed")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a crashed run's directory survived the next run")
	}
	if err := os.Mkdir(filepath.Join(o.Dir, "lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(filepath.Join(o.Dir, "lock"), start, start)
	src := &Fake{}
	if _, err := Run(context.Background(), src, o); !errors.Is(err, ErrLocked) || len(src.Specs) != 0 {
		t.Errorf("a held lock = %v, %d plugin runs", err, len(src.Specs))
	}
}
