package learn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/store"
)

func ev(kind events.Kind, text string) events.Event {
	return events.Event{Harness: "claude", Session: "s1", Timestamp: "2026-11-01T00:00:01Z", Kind: kind, Text: text}
}

func TestRenderCompression(t *testing.T) {
	long := strings.Repeat("words and more words ", 60) // > 500 chars
	evs := []events.Event{
		ev(events.UserPrompt, "fix the bug in parse"),
		{Kind: events.AssistantText, Text: "Here is code:\n```go\nfunc main() {}\n```\nand " + long},
		{Kind: events.ToolCall, Tool: "Bash"},
		{Kind: events.ToolResult, Tool: "Bash", OK: events.Bool(false), Text: "exit 1: " + strings.Repeat("x", 300)},
		{Kind: events.ToolResult, Tool: "Bash", OK: events.Bool(true), Text: "fine"},
		{Kind: events.SkillInvoked, Skill: "recur"},
		{Kind: events.SlashCommand, Skill: "archive-chats", Text: "setup"},
		{Kind: events.UserPrompt, Text: "subagent noise", Sidechain: true},
		{Kind: events.Compact},
	}
	got := Render(evs)
	for _, want := range []string{
		"[owner] fix the bug in parse",
		"[code block]",
		"[…]",
		"[tool: Bash]",
		"[tool Bash FAILED: …",
		"[skill: recur]",
		"[command: /archive-chats setup]",
		"[earlier turns compacted]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render misses %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "subagent noise") {
		t.Error("sidechain events rendered")
	}
	if strings.Contains(got, "fine") {
		t.Error("successful tool results rendered; only failures carry signal")
	}
	if strings.Contains(got, long) {
		t.Error("assistant text not truncated")
	}
}

func TestRenderKeepsContextBeforeShortCorrections(t *testing.T) {
	long := strings.Repeat("sentence about the approach. ", 40) // ~1160 chars
	evs := []events.Event{
		{Kind: events.AssistantText, Text: long},
		ev(events.UserPrompt, "no, shorter"),
		{Kind: events.AssistantText, Text: long},
		ev(events.UserPrompt, "now write a long design doc for the following twelve requirements "+strings.Repeat("requirement ", 60)),
	}
	got := Render(evs)
	parts := strings.Split(got, "[owner]")
	if len(parts[0]) < 1000 {
		t.Errorf("assistant text before a short correction was truncated to %d chars", len(parts[0]))
	}
	if len(parts[1]) > 1000 {
		t.Errorf("assistant text before a long prompt kept %d chars; want the 500 budget", len(parts[1]))
	}
}

const mapOutput = `Some preamble the model added.
Quote: "use recall config set, never edit the file"
Observation[correction]: Config writes go through recall config set, never hand edits to generated files.
Observation[tool-failure]: go test fails twice with the same missing GOPROXY; the proxy must be started first.
Observation[not-a-category]: dropped
Observation without brackets: dropped
Rule[r-0001]: violated — the reply hand-edited a generated file
Rule[r-0002]: followed
Rule[r-9999]: relevant — unknown id, ledger drops it
NONE
`

func TestParseObservations(t *testing.T) {
	obs, hits := ParseObservations(mapOutput)
	if len(obs) != 2 {
		t.Fatalf("observations = %+v", obs)
	}
	if obs[0].Category != "correction" || obs[0].Quote == "" || !strings.Contains(obs[0].Text, "recall config set") {
		t.Errorf("obs[0] = %+v", obs[0])
	}
	if obs[1].Category != "tool-failure" || obs[1].Quote != "" {
		t.Errorf("obs[1] = %+v; quotes must not leak across observations", obs[1])
	}
	if len(hits) != 3 || hits[0].Status != "violated" || hits[0].ID != "r-0001" || hits[1].Note != "" {
		t.Errorf("hits = %+v", hits)
	}
	if obs, hits := ParseObservations("NONE\n"); len(obs)+len(hits) != 0 {
		t.Errorf("NONE parsed to %v %v", obs, hits)
	}
}

func mapper(st store.Store, runner model.Runner, rules []Rule) *Mapper {
	return &Mapper{
		Store: st, Runner: runner, Model: "haiku", Host: "laptop-a", Rules: rules,
		Clock: &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)},
	}
}

func TestMapSessionCaches(t *testing.T) {
	st := store.NewMem()
	fake := &model.Fake{Response: "Observation[context]: The owner's repos live under a GOPATH-style tree.\n"}
	m := mapper(st, fake, nil)
	ctx := context.Background()
	evs := []events.Event{ev(events.UserPrompt, "clone it under the import path")}

	so, ran, err := m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || !ran || len(so.Observations) != 1 {
		t.Fatalf("first map: ran=%v err=%v so=%+v", ran, err, so)
	}
	if !strings.Contains(fake.Calls[0].Input, "[owner] clone it under the import path") {
		t.Error("model input misses the rendered session")
	}

	so2, ran, err := m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || ran {
		t.Fatalf("second map ran the model again: ran=%v err=%v", ran, err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("model calls = %d, want 1", len(fake.Calls))
	}
	if so2.Observations[0] != so.Observations[0] {
		t.Error("cached observations differ")
	}

	// A changed session re-observes under a new fingerprint.
	evs = append(evs, ev(events.UserPrompt, "and set upstream, not origin"))
	_, ran, err = m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || !ran || len(fake.Calls) != 2 {
		t.Fatalf("changed session: ran=%v calls=%d err=%v", ran, len(fake.Calls), err)
	}

	// New rules shown to the model also re-observe.
	m.Rules = []Rule{{ID: "r-0001", Text: "Never push without asking."}}
	_, ran, err = m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || !ran {
		t.Fatalf("rule change: ran=%v err=%v", ran, err)
	}
	if !strings.Contains(fake.Calls[2].Input, "Rule[r-0001]: Never push without asking.") {
		t.Error("model input misses the current rules")
	}

	// The newest observation wins when a session has several fingerprints.
	window, err := ListObservations(ctx, st, "claude")
	if err != nil || len(window) != 1 {
		t.Fatalf("window = %+v, %v; want one deduped session", window, err)
	}
}

func TestMapSessionEmptyRenderSkipsModel(t *testing.T) {
	st := store.NewMem()
	fake := &model.Fake{Response: "unused"}
	m := mapper(st, fake, nil)
	evs := []events.Event{{Kind: events.UserPrompt, Text: "noise", Sidechain: true}}
	so, ran, err := m.MapSession(context.Background(), "claude", "s1", evs)
	if err != nil || ran || len(so.Observations) != 0 || len(fake.Calls) != 0 {
		t.Fatalf("empty render: ran=%v calls=%d err=%v", ran, len(fake.Calls), err)
	}
}

func writeTarget(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLedgerSyncTargets(t *testing.T) {
	dir := t.TempDir()
	target := writeTarget(t, dir,
		"# My rules",
		"- Never push without asking.",
		"* One sentence per line in Markdown.",
		"  - an indented detail bullet, not a rule",
		"```sh",
		"- not a rule, it's inside a code block",
		"```",
		"prose line",
	)
	l := &Ledger{}
	today := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	added, removed, err := l.SyncTargets([]string{target}, today)
	if err != nil || added != 2 || removed != 0 {
		t.Fatalf("added=%d removed=%d err=%v", added, removed, err)
	}
	if l.Rules[0].ID != "r-0001" || l.Rules[1].ID != "r-0002" || l.Rules[0].Created != "2026-11-11" {
		t.Errorf("rules = %+v", l.Rules)
	}

	// Idempotent.
	added, removed, _ = l.SyncTargets([]string{target}, today)
	if added+removed != 0 {
		t.Errorf("resync: added=%d removed=%d", added, removed)
	}

	// A deleted bullet leaves the ledger; a new one gets the next id.
	writeTarget(t, dir, "- One sentence per line in Markdown.", "- Rebase, never merge.")
	added, removed, _ = l.SyncTargets([]string{target}, today)
	if added != 1 || removed != 1 {
		t.Fatalf("after edit: added=%d removed=%d %+v", added, removed, l.Rules)
	}
	if l.Rules[len(l.Rules)-1].ID != "r-0003" {
		t.Errorf("new rule id = %s, want r-0003 (ids never reused)", l.Rules[len(l.Rules)-1].ID)
	}

	// Round trip.
	path := filepath.Join(dir, "ledger.json")
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadLedger(path)
	if err != nil || len(back.Rules) != len(l.Rules) {
		t.Fatalf("round trip: %v, %d rules", err, len(back.Rules))
	}
	if empty, err := LoadLedger(filepath.Join(dir, "missing.json")); err != nil || len(empty.Rules) != 0 {
		t.Errorf("missing ledger: %+v, %v", empty, err)
	}
}

func TestApplyHitsAndAge(t *testing.T) {
	l := &Ledger{Rules: []Rule{
		{ID: "r-0001", Text: "fresh", Created: "2026-11-01"},
		{ID: "r-0002", Text: "earned", Created: "2026-01-01"},
		{ID: "r-0003", Text: "violated", Created: "2026-01-01"},
		{ID: "r-0004", Text: "stale", Created: "2026-01-01", LastRelevant: "2026-03-01"},
	}}
	l.ApplyHits([]SessionObservations{{
		ObservedAt: "2026-11-05T10:00:00Z",
		RuleHits: []RuleHit{
			{ID: "r-0002", Status: "followed"},
			{ID: "r-0003", Status: "violated"},
			{ID: "r-9999", Status: "violated"}, // invented id: dropped
		},
	}})
	if l.Rules[1].LastRelevant != "2026-11-05" {
		t.Errorf("followed rule: %+v", l.Rules[1])
	}
	if l.Rules[2].LastViolated != "2026-11-05" || l.Rules[2].LastRelevant != "2026-11-05" {
		t.Errorf("violated rule: %+v", l.Rules[2])
	}
	a := l.Age(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), 90)
	if len(a.Reword) != 1 || a.Reword[0].ID != "r-0003" {
		t.Errorf("reword = %+v", a.Reword)
	}
	if len(a.Retire) != 1 || a.Retire[0].ID != "r-0004" {
		t.Errorf("retire = %+v", a.Retire)
	}
}

func TestReducerBuildsInputAndWritesProposals(t *testing.T) {
	ledger := &Ledger{Rules: []Rule{
		{ID: "r-0001", Target: "/home/you/.claude/CLAUDE.md", Text: "Never push without asking.",
			Created: "2026-01-01", LastRelevant: "2026-11-01"},
		{ID: "r-0002", Target: "/home/you/.claude/CLAUDE.md", Text: "stale rule",
			Created: "2026-01-01", LastRelevant: "2026-02-01"},
	}}
	window := []SessionObservations{{
		Session: "s1", ObservedAt: "2026-11-05T00:00:00Z",
		Observations: []Observation{{Category: "correction", Text: "Quote PRs in full.", Quote: "quote it"}},
	}}
	fake := &model.Fake{Response: "## Proposals\n\n### retire: stale rule\n- evidence: s1\n",
		Usage: model.Usage{InputTokens: 1000, OutputTokens: 50, CostUSD: 0.05}}
	r := &Reducer{Runner: fake, Model: "opus", Ledger: ledger, RetireDays: 90,
		Clock: &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}}
	out := t.TempDir()
	path, usage, err := r.Run(context.Background(), window, out)
	if err != nil {
		t.Fatal(err)
	}
	if usage.CostUSD != 0.05 {
		t.Errorf("usage = %+v, want the fake's cost", usage)
	}
	input := fake.Calls[0].Input
	for _, want := range []string{
		"Rule[r-0001]", "Never push without asking.",
		"Observation[correction] (session s1): Quote PRs in full.",
		`Quote: "quote it"`,
		"retire Rule[r-0002]",
	} {
		if !strings.Contains(input, want) {
			t.Errorf("reduce input misses %q", want)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "### retire: stale rule") {
		t.Errorf("proposals = %s, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(out, "input.md")); err != nil {
		t.Error("input.md not written beside proposals")
	}
}

func TestNameCollisions(t *testing.T) {
	out := "## Proposals\n\n" +
		"### skill: Resume a stalled run\n- target: new skill\n- name: resume\n- rule: x\n\n" +
		"### add: A rule\n- target: CLAUDE.md\n- name: keep\n\n" + // not a skill proposal
		"### skill: Advisory drafting\n- target: new skill\n- name: `/advisory-draft`\n\n" +
		"### skill: Keep, changed\n- target: skills/keep/SKILL.md\n"
	got := NameCollisions(out, []string{"keep", "Resume", "plugin"})
	if strings.Join(got, ",") != "resume" {
		t.Errorf("collisions = %v, want [resume]", got)
	}
}

// refuser refuses any input containing a flagged word, like a safeguard.
type refuser struct{ calls int }

func (r *refuser) Run(ctx context.Context, model_, input string) (string, model.Usage, error) {
	r.calls++
	if strings.Contains(input, "FLAGGED") {
		return "", model.Usage{CostUSD: 0.01}, fmt.Errorf("claude -p: %w: flagged", model.ErrRefusal)
	}
	return "Observation[context]: saw " + fmt.Sprint(strings.Count(input, "[owner] ")) + " prompts\n", model.Usage{CostUSD: 0.01}, nil
}

func promptsSession(flagged int, refusalAfter int) []events.Event {
	var evs []events.Event
	for i := 0; i < 6; i++ {
		text := fmt.Sprintf("prompt %d", i)
		if i == flagged {
			text += " FLAGGED"
		}
		evs = append(evs, ev(events.UserPrompt, text), ev(events.AssistantText, "ok"))
		if i == refusalAfter {
			evs = append(evs, events.Event{Kind: events.Refusal, Text: "cyber"})
		}
	}
	return evs
}

func TestMapSalvagesRefusedSession(t *testing.T) {
	cases := []struct {
		name                   string
		flagged, refusalAfter  int
		wantPrompts, wantCalls int
	}{
		// The session's own refusal points at the flagged prompt: one retry.
		{"refusal in session", 3, 3, 3, 2},
		// No refusal recorded: binary search finds the prefix before it.
		{"binary search", 2, -1, 2, 4},
		// Flagged from the first prompt: nothing to salvage, still cached.
		{"nothing salvageable", 0, -1, 0, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &refuser{}
			m := mapper(store.NewMem(), r, nil)
			evs := promptsSession(tc.flagged, tc.refusalAfter)
			so, ran, err := m.MapSession(context.Background(), "claude", "s1", evs)
			if err != nil || !ran {
				t.Fatalf("ran=%v err=%v", ran, err)
			}
			if so.Partial == nil || so.Partial.Prompts != tc.wantPrompts || so.Partial.Of != 6 {
				t.Errorf("partial = %+v, want %d of 6", so.Partial, tc.wantPrompts)
			}
			if r.calls != tc.wantCalls {
				t.Errorf("calls = %d, want %d", r.calls, tc.wantCalls)
			}
			if tc.wantPrompts > 0 && (len(so.Observations) != 1 || !strings.Contains(so.Observations[0].Text, fmt.Sprintf("saw %d prompts", tc.wantPrompts))) {
				t.Errorf("observations = %+v", so.Observations)
			}
			if so.CostUSD != 0.01*float64(tc.wantCalls) {
				t.Errorf("cost = %v, want every attempt counted", so.CostUSD)
			}
			if _, ran, _ := m.MapSession(context.Background(), "claude", "s1", evs); ran {
				t.Error("a salvaged session should be cached")
			}
		})
	}
}

func TestWouldCall(t *testing.T) {
	ctx := context.Background()
	m := mapper(store.NewMem(), &model.Fake{Response: "NONE\n"}, nil)
	evs := []events.Event{ev(events.UserPrompt, "do the thing")}
	if call, err := m.WouldCall(ctx, "claude", "s1", evs); err != nil || !call {
		t.Fatalf("new session: call=%v err=%v", call, err)
	}
	if _, _, err := m.MapSession(ctx, "claude", "s1", evs); err != nil {
		t.Fatal(err)
	}
	if call, _ := m.WouldCall(ctx, "claude", "s1", evs); call {
		t.Error("a cached session would call the model again")
	}
	if call, _ := m.WouldCall(ctx, "claude", "s2", []events.Event{ev(events.AssistantText, "hi")}); call {
		t.Error("a session with no owner prompts would call the model")
	}
}

func TestOffTargets(t *testing.T) {
	out := "## Proposals\n\n" +
		"### add: In target, tilde form\n- target: ~/ctx/CLAUDE.md\n\n" +
		"### reword: In target, absolute\n- target: `/home/you/ctx/CLAUDE.md`\n\n" +
		"### add: Elsewhere\n- target: /home/you/.claude/base.md\n\n" +
		"### retire: Named loosely\n- target: agent-kit CLAUDE.md\n\n" +
		"### skill: A skill\n- target: skills/keep/SKILL.md\n\n" +
		"### graduate: A script\n- target: bin/md-lint\n"
	got := OffTargets(out, []string{"/home/you/ctx/CLAUDE.md"}, "/home/you")
	if len(got) != 2 || got[0].Title != "Elsewhere" || got[1].Target != "agent-kit CLAUDE.md" {
		t.Errorf("off targets = %+v", got)
	}
	if OffTargets(out, nil, "/home/you") != nil {
		t.Error("no configured targets should flag nothing")
	}
}
