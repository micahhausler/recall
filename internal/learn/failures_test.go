package learn

import (
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/events"
)

// benignFailure separates expected nonzero exits from mistakes, judged from
// the command and its output. The shapes mirror real transcripts: a failed
// Bash result opens with "Exit code N", and a chain's status is its last
// command's.
func TestBenignFailure(t *testing.T) {
	for _, tc := range []struct {
		name, tool, command, output string
		want                        bool
	}{
		{"no-match grep", "Bash", `grep -rn "window" docs/`, "Exit code 1", true},
		{"no-match rg", "Bash", "rg deadline internal/", "Exit code 1\n", true},
		{"no-match grep after a pipe", "Bash", "ls ~/bin | grep -i raw", "Exit code 1", true},
		{"grep by full path", "Bash", "/usr/bin/grep -c TODO main.go", "Exit code 1", true},
		{"diff found differences", "Bash", "diff want.txt got.txt", "Exit code 1\n3c3\n< a\n---\n> b", true},
		{"false test", "Bash", `test -f config.d/90-set.local.toml`, "Exit code 1", true},
		{"false bracket", "Bash", `[ -d .worktree ]`, "Exit code 1", true},
		{"grep bad pattern", "Bash", `grep -P "(?<" main.go`, "Exit code 2\ngrep: missing )", false},
		{"grep over a missing file", "Bash", "grep TODO gone.go", "Exit code 2\ngrep: gone.go: No such file or directory", false},
		{"diff unreadable file", "Bash", "diff a.txt gone.txt", "Exit code 2\ndiff: gone.txt: No such file or directory", false},
		{"earlier command in the chain failed", "Bash", "which raw; grep raw PATH.txt",
			"Exit code 1\n/usr/bin/which: no raw in (/home/you/bin)", false},
		{"chain ending elsewhere", "Bash", "ls ~/bin | grep -i raw; which -a raw",
			"Exit code 1\n/usr/bin/which: no raw in (/home/you/bin)", false},
		{"zsh glob with no match", "Bash", "ls .github/PULL_REQUEST_TEMPLATE*",
			"Exit code 1\n(eval):1: no matches found: .github/PULL_REQUEST_TEMPLATE*", false},
		{"missing file printed", "Bash", "cat /home/you/gone.yaml", "Exit code 1\ncat: /home/you/gone.yaml: No such file or directory", false},
		{"not a shell tool", "Read", "/home/you/gone.txt", "File does not exist.", false},
		{"no command recorded", "Bash", "", "Exit code 1", false},
		{"no exit code line, silent grep", "Bash", "grep TODO main.go", "", true},
	} {
		if got := benignFailure(tc.tool, tc.command, tc.output); got != tc.want {
			t.Errorf("%s: benignFailure(%q, %q, %q) = %v, want %v", tc.name, tc.tool, tc.command, tc.output, got, tc.want)
		}
	}
}

// Done-when: a rendered session with a no-match grep shows no FAILED line
// for it, while a real failure still renders.
func TestRenderSkipsBenignFailures(t *testing.T) {
	evs := []events.Event{
		{Kind: events.UserPrompt, Text: "find the config loader"},
		{Kind: events.ToolResult, Tool: "Bash", Input: `grep -rn "loadConfig" internal/`, OK: events.Bool(false), Text: "Exit code 1"},
		{Kind: events.ToolResult, Tool: "Bash", Input: "go vet ./...", OK: events.Bool(false), Text: "Exit code 1\ninternal/config/schema.go:10: undefined: foo"},
	}
	got := Render(evs)
	if strings.Contains(got, "loadConfig") {
		t.Errorf("a no-match grep rendered as FAILED:\n%s", got)
	}
	if !strings.Contains(got, "[tool Bash FAILED on `go vet ./...`") {
		t.Errorf("a real failure must still render:\n%s", got)
	}
}

func TestFailuresOfNormalizesAndSkipsBenign(t *testing.T) {
	cwd := "/home/you/src/project"
	result := func(tool, input, text string) events.Event {
		return events.Event{Kind: events.ToolResult, Tool: tool, Input: input, OK: events.Bool(false), Text: text, CWD: cwd}
	}
	evs := []events.Event{
		result("Bash", "go vet ./...", "Exit code 1\n/home/you/src/project/main.go:3: undefined: foo"),
		result("Bash", "go vet ./...", "Exit code 1\n/home/you/src/project/main.go:3: undefined: foo"), // same failure once
		result("Bash", "grep -c TODO main.go", "Exit code 1"),                                          // benign
		{Kind: events.ToolResult, Tool: "Bash", Input: "x", OK: events.Bool(false), Text: "boom", Sidechain: true},
		{Kind: events.ToolResult, Tool: "Bash", Input: "ls", OK: events.Bool(true), Text: "fine"},
	}
	got := failuresOf(evs, "/home/you")
	want := Signal{Tool: "Bash", Input: "Exit code 1 main.go:3: undefined: foo"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("failures = %+v, want [%+v]", got, want)
	}
}

// Done-when: with learn.repeat_evidence_sessions at 3, the same failure in
// three sessions becomes a tool-derived candidate that can support an add on
// its own; at 0, it is never counted and an add citing nothing owner-typed
// is held back.
func TestFailureRepeatSupportsAnAdd(t *testing.T) {
	failure := Signal{Tool: "Bash", Input: "Exit code 127 zsh: command not found: $CMD"}
	in := func(id string) SessionObservations {
		so := session(id, "2026-11-01")
		so.Prompts = 1
		so.Failures = []Signal{failure}
		return so
	}
	window := []SessionObservations{in("a"), in("b"), in("c")}
	clk := &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	reducer := func(key int) *Reducer {
		return &Reducer{Ledger: &Ledger{}, Clock: clk, RetireDays: 90,
			Candidates: &CandidateLedger{}, RepeatEvidenceSessions: key}
	}

	r := reducer(3)
	open, _ := r.clusters(window)
	if len(open) != 1 || open[0].Sessions != 3 || open[0].Category != CategoryRepeat {
		t.Fatalf("clusters = %+v, want one failure repeat across 3 sessions", open)
	}
	if text := r.CandidatesText(window); !strings.Contains(text, "tool-derived") {
		t.Errorf("the candidate isn't labeled tool-derived:\n%s", text)
	}
	add := Proposal{Kind: "add", Title: "Quote or split shell variables", Candidates: []string{open[0].ID}}
	ev := runEvidence(open)
	if !supported(add, ev, 3) {
		t.Error("with the key at 3, a failure in three sessions must support the add")
	}
	if held := Unsupported([]Proposal{add}, ev, 3); len(held) != 0 {
		t.Errorf("the add was held back: %+v", held)
	}
	// The expanded evidence line carries the label into the proposal.
	if out := ExpandEvidence("- evidence: "+open[0].ID, open); !strings.Contains(out, "tool-derived repeats") {
		t.Errorf("expanded evidence isn't labeled tool-derived: %q", out)
	}

	// At 0, failures aren't counted at all, and an add resting on nothing
	// owner-typed is held back.
	r0 := reducer(0)
	if open, _ := r0.clusters(window); len(open) != 0 {
		t.Fatalf("with the key off, failures were counted: %+v", open)
	}
	if held := Unsupported([]Proposal{add}, RunEvidence{}, 0); len(held) != 1 {
		t.Errorf("with the key off, the add must be held back: %+v", held)
	}
}
