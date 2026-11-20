package learn

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/store"
)

// Two machines keep notes under the same project and name, so their records
// share one session key. Both hosts' evidence must survive the window.
func TestMemoryNotesSurvivePerHost(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	noteA := harness.Memory{
		Project: "p", Name: "zsh", Session: "s-a", Modified: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
		Description: "zsh expands a leading equals sign, so quote separator lines",
	}
	noteB := noteA
	noteB.Session, noteB.Modified = "s-b", time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	noteB.Description = "quote separator lines in Bash commands"
	if _, err := PutMemories(ctx, st, []harness.Memory{noteA}, "claude", "laptop-a", "/home/you", nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := PutMemories(ctx, st, []harness.Memory{noteB}, "claude", "laptop-b", "/home/you", nil, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	window, err := ListObservations(ctx, st, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(window) != 2 {
		t.Fatalf("window keeps %d record(s), want both hosts' notes: %+v", len(window), window)
	}
	hosts := map[string]bool{}
	for _, so := range window {
		hosts[so.Host] = true
	}
	if !hosts["laptop-a"] || !hosts["laptop-b"] {
		t.Errorf("hosts in window = %v, want laptop-a and laptop-b", hosts)
	}
}

func TestMemoryNotesBecomeObservations(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	note := harness.Memory{
		Path: "/home/you/.claude/projects/p/memory/zsh.md", Project: "p", CWD: "/home/you/src/project",
		Name: "zsh", Type: "feedback", Session: "s1", Modified: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
		Description: "zsh expands a leading equals sign, so separator lines must be quoted in Bash commands",
	}
	empty := harness.Memory{Project: "p", Name: "empty", Modified: now}
	locate := func(string) (string, string) { return "/home/you/src/project", "example.com/you/project" }
	put := func(mems ...harness.Memory) int {
		t.Helper()
		n, err := PutMemories(ctx, st, mems, "claude", "laptop-a", "/home/you", locate, now)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := put(note, empty); n != 1 {
		t.Fatalf("stored %d records, want the one note that says something", n)
	}
	if n := put(note); n != 0 {
		t.Errorf("an unchanged note was stored again: %d", n)
	}
	window, err := ListObservations(ctx, st, "claude")
	if err != nil || len(window) != 1 {
		t.Fatalf("window = %+v, %v", window, err)
	}
	so := window[0]
	want := Scope{CWD: "~/src/project", Repo: "~/src/project", Remote: "example.com/you/project"}
	if so.Session != "memory:p/zsh" || so.Day() != "2026-11-02" || so.Scope != want || so.Host != "laptop-a" {
		t.Errorf("record = %+v", so)
	}
	if o := so.Observations[0]; o.Category != CategoryMemory || o.Origin != "s1" || o.Source != "" ||
		!strings.HasPrefix(o.Text, "(feedback) zsh expands") {
		t.Errorf("observation = %+v", o)
	}

	// The note and the session it came from count once; another session
	// makes it two.
	origin := session("s1", "2026-11-01", zshA)
	other := session("s2", "2026-11-03", zshB)
	cl := &CandidateLedger{}
	open, _ := Open(cl.Cluster(append(filler(), origin, other, so), "2026-08-13", "2026-11-11"), nil)
	k := open[0]
	if len(k.Evidence) != 3 || k.Sessions != 2 {
		t.Fatalf("cluster = %d observation(s) from %d session(s), want 3 from 2", len(k.Evidence), k.Sessions)
	}
	var sb strings.Builder
	renderCandidates(&sb, open[:1], 0)
	if !strings.Contains(sb.String(), "- [memory] 2026-11-02 note p/zsh from session s1 in example.com/you/project:") {
		t.Errorf("a note should be shown as a note:\n%s", sb.String())
	}
	if refs := runEvidence(open[:1])[k.ID]; len(refs) != 2 {
		t.Errorf("evidence record lists %d sessions, want 2: %+v", len(refs), refs)
	}

	// A changed note is a new record, and the newest wins.
	note.Description = "zsh expands a leading equals sign; quote separator lines"
	note.Modified = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	now = now.Add(time.Hour)
	if n := put(note); n != 1 {
		t.Fatalf("a changed note wasn't stored: %d", n)
	}
	if window, _ = ListObservations(ctx, st, "claude"); len(window) != 1 || window[0].Day() != "2026-11-09" {
		t.Errorf("after a change: %+v", window)
	}
}

// A dotfiles-synced note exists on every machine, each under that machine's
// own project key, and every host's record survives the window. One logical
// note must count as one session toward any threshold
// (learn.repeat_evidence_sessions), never once per host.
func TestSyncedMemoryNoteCountsOnceAcrossHosts(t *testing.T) {
	modified := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	var window []SessionObservations
	for _, host := range []string{"laptop-a", "laptop-b", "laptop-c"} {
		note := harness.Memory{
			Project: "-home-" + host + "-dotfiles", Name: "zsh", Modified: modified,
			Description: "zsh does not word-split unquoted variables",
		}
		so, ok := MemoryRecord(note, "claude", host, "/home/you", nil, modified)
		if !ok {
			t.Fatal("note yielded no record")
		}
		window = append(window, so)
	}
	cl := &CandidateLedger{}
	clusters := cl.Cluster(window, "2026-10-01", "2026-11-02")
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want the one note's: %+v", len(clusters), clusters)
	}
	if k := clusters[0]; k.Sessions != 1 || len(k.Evidence) != 3 {
		t.Errorf("one synced note counts as %d session(s) over %d record(s), want 1 over 3", k.Sessions, len(k.Evidence))
	}
	if refs := runEvidence(clusters)[clusters[0].ID]; len(refs) != 1 {
		t.Errorf("evidence refs = %d, want 1: %+v", len(refs), refs)
	}
}

// Notes with different words are different lessons: each keeps its own
// session identity, however alike their wording, so genuinely distinct
// notes still count separately.
func TestDistinctMemoryNotesCountSeparately(t *testing.T) {
	modified := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	texts := map[string]string{
		"laptop-a": "Quote separator lines in zsh commands",
		"laptop-b": "Quote separator lines in zsh commands, always",
	}
	var window []SessionObservations
	for _, host := range []string{"laptop-a", "laptop-b"} {
		note := harness.Memory{
			Project: "-home-" + host + "-dotfiles", Name: "zsh", Modified: modified,
			Description: texts[host],
		}
		so, ok := MemoryRecord(note, "claude", host, "/home/you", nil, modified)
		if !ok {
			t.Fatal("note yielded no record")
		}
		window = append(window, so)
	}
	cl := &CandidateLedger{}
	clusters := cl.Cluster(window, "2026-10-01", "2026-11-02")
	if len(clusters) != 1 {
		t.Fatalf("clusters = %d, want the alike notes joined: %+v", len(clusters), clusters)
	}
	if k := clusters[0]; k.Sessions != 2 {
		t.Errorf("two distinct notes count as %d session(s), want 2", k.Sessions)
	}
}
