package learn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
		{Kind: events.ToolResult, Tool: "Bash", Input: "go test ./...", OK: events.Bool(false), Text: "exit 1: " + strings.Repeat("x", 300)},
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
		"[tool Bash FAILED on `go test ./...`: …",
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

	res, err := m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || !res.Observed || res.Judged || len(res.Observations.Observations) != 1 {
		t.Fatalf("first map: err=%v res=%+v", err, res)
	}
	if !strings.Contains(fake.Calls[0].Input, "[owner] clone it under the import path") {
		t.Error("model input misses the rendered session")
	}
	if strings.Contains(fake.Calls[0].Input, "CURRENT RULES") || strings.Contains(fake.Calls[0].Input, "# Rule hits") {
		t.Error("with no rules, the input should ask for no rule hits")
	}

	res2, err := m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || res2.Ran() {
		t.Fatalf("second map ran the model again: err=%v res=%+v", err, res2)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("model calls = %d, want 1", len(fake.Calls))
	}
	if res2.Observations.Observations[0] != res.Observations.Observations[0] {
		t.Error("cached observations differ")
	}

	// A changed session re-observes under a new fingerprint.
	evs = append(evs, ev(events.UserPrompt, "and set upstream, not origin"))
	res, err = m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || !res.Observed || len(fake.Calls) != 2 {
		t.Fatalf("changed session: calls=%d err=%v", len(fake.Calls), err)
	}

	// The newest observation wins when a session has several fingerprints.
	window, err := ListObservations(ctx, st, "claude")
	if err != nil || len(window) != 1 {
		t.Fatalf("window = %+v, %v; want one deduped session", window, err)
	}
}

// countingStore wraps a Store and counts read round-trips.
type countingStore struct {
	store.Store
	lists, gets, heads int
}

func (c *countingStore) List(ctx context.Context, prefix string) ([]string, error) {
	c.lists++
	return c.Store.List(ctx, prefix)
}

func (c *countingStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.gets++
	return c.Store.Get(ctx, key)
}

func (c *countingStore) Head(ctx context.Context, key string) (store.Info, error) {
	c.heads++
	return c.Store.Head(ctx, key)
}

// A fully cached map must not list and read every prior judgment per
// session: one Head of the expected hits key answers it.
func TestFullyCachedMapSkipsHitListings(t *testing.T) {
	st := store.NewMem()
	fake := &model.Fake{Response: "Observation[context]: Repos live under a GOPATH-style tree.\nRule[r-0001]: followed\n"}
	rule := Rule{ID: "r-0001", Text: "Never push without asking.", TextHash: textHash("Never push without asking."), Created: "2026-01-01"}
	ctx := context.Background()
	const n = 5
	sessions := map[string][]events.Event{}
	for i := 0; i < n; i++ {
		s := fmt.Sprintf("s%d", i)
		sessions[s] = []events.Event{{Harness: "claude", Session: s, Timestamp: "2026-11-01T00:00:01Z",
			Kind: events.UserPrompt, Text: "prompt " + s}}
	}
	m := mapper(st, fake, []Rule{rule})
	for s, evs := range sessions {
		if res, err := m.MapSession(ctx, "claude", s, evs); err != nil || !res.Observed || !res.Judged {
			t.Fatalf("first map of %s: err=%v res=%+v", s, err, res)
		}
	}

	cs := &countingStore{Store: st}
	m = mapper(cs, fake, []Rule{rule})
	calls := len(fake.Calls)
	for s, evs := range sessions {
		if res, err := m.MapSession(ctx, "claude", s, evs); err != nil || res.Ran() {
			t.Fatalf("cached map of %s ran the model: err=%v res=%+v", s, err, res)
		}
	}
	if len(fake.Calls) != calls {
		t.Fatalf("cached pass made %d model call(s)", len(fake.Calls)-calls)
	}
	if cs.lists != 0 {
		t.Errorf("cached pass listed the store %d time(s), want 0", cs.lists)
	}
	if cs.gets != n {
		t.Errorf("cached pass read %d object(s), want %d (one cached observation per session)", cs.gets, n)
	}
	if cs.heads > n {
		t.Errorf("cached pass made %d Head call(s), want at most %d", cs.heads, n)
	}
}

// A rule change never re-observes a session, and a session is judged only
// against rules that existed while it ran.
func TestRuleChangeNeverReobserves(t *testing.T) {
	st := store.NewMem()
	fake := &model.Fake{Response: "Observation[context]: Repos live under a GOPATH-style tree.\n" +
		"Rule[r-0001]: violated — pushed unasked\nRule[r-9999]: relevant — an id the model made up\n"}
	old := Rule{ID: "r-0001", Text: "Never push without asking.", TextHash: textHash("Never push without asking."), Created: "2026-01-01"}
	m := mapper(st, fake, []Rule{old})
	ctx := context.Background()
	evs := []events.Event{ev(events.UserPrompt, "ship it")} // ran 2026-11-01

	res, err := m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || !res.Observed || !res.Judged {
		t.Fatalf("first map: err=%v res=%+v", err, res)
	}
	in := fake.Calls[0].Input
	if !strings.Contains(in, "# Rule hits") || !strings.Contains(in, "Rule[r-0001]: Never push without asking.") {
		t.Error("one call should observe and judge: the input misses the rule-hit prompt or the rules")
	}
	if len(res.Observations.RuleHits) != 0 {
		t.Error("rule hits belong in their own record, not the observation record")
	}
	if h := res.Hits; len(h.Hits) != 1 || h.Hits[0].TextHash != old.TextHash || h.Day() != "2026-11-01" || len(h.Rules) != 1 {
		t.Errorf("hits = %+v", h)
	}

	// A bullet added after the session ran: nothing to observe, nothing to
	// judge, no model call.
	added := Rule{ID: "r-0002", Text: "Rebase, never merge.", TextHash: textHash("Rebase, never merge."),
		Created: "2026-11-05", Since: "2026-11-05"}
	m.Rules = []Rule{old, added}
	if observe, judge, err := m.WouldCall(ctx, "claude", "s1", evs); err != nil || observe || judge {
		t.Errorf("a rule written after the session would call the model: observe=%v judge=%v err=%v", observe, judge, err)
	}
	if res, err := m.MapSession(ctx, "claude", "s1", evs); err != nil || res.Ran() || len(fake.Calls) != 1 {
		t.Fatalf("adding a later rule ran the model: err=%v calls=%d", err, len(fake.Calls))
	}

	// A rule that was already in a file when the ledger first read it is
	// judged, alone, without re-observing.
	found := Rule{ID: "r-0003", Text: "One sentence per line.", TextHash: textHash("One sentence per line."), Created: "2026-11-05"}
	m.Rules = []Rule{old, added, found}
	fake.Response = "Rule[r-0003]: followed\n"
	res, err = m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || res.Observed || !res.Judged || len(fake.Calls) != 2 {
		t.Fatalf("catch-up judgment: err=%v res=%+v calls=%d", err, res, len(fake.Calls))
	}
	in = fake.Calls[1].Input
	if !strings.Contains(in, "# Judge one session against the rules") || strings.Contains(in, "# Observe one session") ||
		!strings.Contains(in, "Rule[r-0003]") || strings.Contains(in, "Rule[r-0001]") || strings.Contains(in, "Rule[r-0002]") {
		t.Errorf("the catch-up input should ask only for hits on the one unjudged rule:\n%s", in)
	}
	if res, _ := m.MapSession(ctx, "claude", "s1", evs); res.Ran() {
		t.Error("a judged rule was judged again")
	}

	// Every judgment of the session reaches the ledger, by session day, and
	// a hit for another wording of the id is dropped.
	judged, err := ListHits(ctx, st, "claude")
	if err != nil || len(judged) != 2 {
		t.Fatalf("judgments = %d, %v", len(judged), err)
	}
	l := &Ledger{Rules: []Rule{old, added, found}}
	l.ApplyHits(nil, judged)
	if l.Rules[0].LastViolated != "2026-11-01" || l.Rules[2].LastRelevant != "2026-11-01" || l.Rules[1].LastRelevant != "" {
		t.Errorf("ledger after hits = %+v", l.Rules)
	}
	other := &Ledger{Rules: []Rule{{ID: "r-0001", Text: "A different ledger's rule.", TextHash: textHash("A different ledger's rule.")}}}
	other.ApplyHits(nil, judged)
	if other.Rules[0].LastViolated != "" {
		t.Error("a hit landed on a different rule that shares the id")
	}
}

// A leaked session recall itself authored (a Kiro runner run saved and not
// deleted) must never count as the owner's: its first prompt is one of
// recall's own model inputs, and mapping it as owner evidence would feed
// the loop its own prompts.
func TestMapSessionSkipsRecallOwnRun(t *testing.T) {
	st := store.NewMem()
	fake := &model.Fake{Response: "Observation[context]: poisoned\n"}
	rule := Rule{ID: "r-0001", Text: "Never push without asking.", TextHash: textHash("Never push without asking."), Created: "2026-01-01"}
	m := mapper(st, fake, []Rule{rule})
	// Exactly what a map run sends: the embedded prompt, then a session.
	leaked := m.input("[owner] clone it under the import path\n", true, []Rule{rule})
	evs := []events.Event{ev(events.UserPrompt, leaked)}
	res, err := m.MapSession(context.Background(), "kiro", "sess_leak", evs)
	if err != nil || res.Ran() || len(fake.Calls) != 0 {
		t.Fatalf("recall's own run was mapped: res=%+v calls=%d err=%v", res, len(fake.Calls), err)
	}
	if n := len(res.Observations.Observations); n != 0 {
		t.Errorf("observations from recall's own run: %d", n)
	}
	// The other inputs the loop sends through a runner are skipped too.
	for i, first := range []string{hitsOnlyPrompt, reducePrompt, applyPrompt, replayPrompt} {
		evs := []events.Event{ev(events.UserPrompt, first+"\n\n# SESSION\n\n[owner] hi\n")}
		if res, err := m.MapSession(context.Background(), "kiro", fmt.Sprintf("sess_leak%d", i), evs); err != nil || res.Ran() {
			t.Errorf("input %d: recall's own run was mapped: res=%+v err=%v", i, res, err)
		}
	}
}

func TestMapSessionEmptyRenderSkipsModel(t *testing.T) {
	st := store.NewMem()
	fake := &model.Fake{Response: "unused"}
	m := mapper(st, fake, nil)
	evs := []events.Event{{Kind: events.UserPrompt, Text: "noise", Sidechain: true}}
	res, err := m.MapSession(context.Background(), "claude", "s1", evs)
	if err != nil || res.Ran() || len(res.Observations.Observations) != 0 || len(fake.Calls) != 0 {
		t.Fatalf("empty render: res=%+v calls=%d err=%v", res, len(fake.Calls), err)
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
	res, err := l.SyncTargets([]string{target}, today)
	if err != nil || res != (Sync{Added: 2}) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if l.Rules[0].ID != "r-0001" || l.Rules[1].ID != "r-0002" || l.Rules[0].Created != "2026-11-11" {
		t.Errorf("rules = %+v", l.Rules)
	}
	if l.Rules[0].Since != "" {
		t.Errorf("since = %q; a bullet already in a file read for the first time has no known start", l.Rules[0].Since)
	}

	// Idempotent.
	if res, _ = l.SyncTargets([]string{target}, today); res != (Sync{}) {
		t.Errorf("resync: %+v", res)
	}

	// A deleted bullet leaves the ledger; a new one gets the next id.
	writeTarget(t, dir, "- One sentence per line in Markdown.", "- Rebase, never merge.")
	res, _ = l.SyncTargets([]string{target}, today.AddDate(0, 0, 1))
	if res != (Sync{Added: 1, Removed: 1}) {
		t.Fatalf("after edit: %+v %+v", res, l.Rules)
	}
	last := l.Rules[len(l.Rules)-1]
	if last.ID != "r-0003" {
		t.Errorf("new rule id = %s, want r-0003 (ids never reused)", last.ID)
	}
	if last.Since != "2026-11-12" {
		t.Errorf("since = %q; a bullet new to a known file starts the day it appears", last.Since)
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

// A reworded rule keeps its id, its dates, and its violation history; only
// a violation of the new wording asks for another reword.
// A Kiro steering file's YAML frontmatter is configuration, not rules: a
// fileMatchPattern list would otherwise read as top-level bullets.
func TestSyncSkipsSteeringFrontmatter(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	text := strings.Join([]string{
		"---",
		"inclusion: fileMatch",
		"fileMatchPattern:",
		"- 'src/**'",
		"- 'test/**'",
		"---",
		"",
		"# Build steering",
		"- Run the build before saying done.",
	}, "\n") + "\n"
	if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &Ledger{}
	res, err := l.SyncTargets([]string{target}, time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC))
	if err != nil || res != (Sync{Added: 1}) {
		t.Fatalf("res=%+v err=%v rules=%+v", res, err, l.Rules)
	}
	if l.Rules[0].Text != "Run the build before saying done." {
		t.Errorf("rule = %+v", l.Rules[0])
	}
	// A --- later in the body is a thematic break, not frontmatter.
	body := "- A rule before the break.\n\n---\n\n- A rule after the break.\n"
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	l = &Ledger{}
	if res, err = l.SyncTargets([]string{target}, time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)); err != nil || res != (Sync{Added: 2}) {
		t.Fatalf("thematic break: res=%+v err=%v rules=%+v", res, err, l.Rules)
	}
}

func TestSyncKeepsRuleAcrossReword(t *testing.T) {
	dir := t.TempDir()
	target := writeTarget(t, dir, "- Never push without asking.", "- Use American spelling in docs.")
	other := filepath.Join(dir, "other.md")
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	l := &Ledger{}
	if _, err := l.SyncTargets([]string{target, other}, day(1)); err != nil {
		t.Fatal(err)
	}
	l.Rules[0].LastRelevant, l.Rules[0].LastViolated = "2026-11-03", "2026-11-03"
	if a := l.Age(day(5), 90); len(a.Reword) != 1 {
		t.Fatalf("violated rule not up for reword: %+v", a)
	}

	// A hand edit that reads like the old rule is the same rule.
	writeTarget(t, dir, "- Never push without asking first.", "- Use American spelling in docs.")
	res, err := l.SyncTargets([]string{target, other}, day(5))
	if err != nil || res != (Sync{Reworded: 1}) {
		t.Fatalf("hand reword: %+v err=%v", res, err)
	}
	r := l.Rules[0]
	if r.ID != "r-0001" || r.Text != "Never push without asking first." || r.Since != "2026-11-05" ||
		r.Created != "2026-11-01" || r.LastViolated != "2026-11-03" {
		t.Errorf("reworded rule = %+v", r)
	}
	if len(r.Supersedes) != 1 || r.Supersedes[0].Text != "Never push without asking." || r.Supersedes[0].Until != "2026-11-05" {
		t.Errorf("supersedes = %+v", r.Supersedes)
	}
	if !r.Wrote(textHash("Never push without asking.")) || r.Wrote(textHash("something else")) {
		t.Error("Wrote should cover the earlier wording and nothing else")
	}
	if a := l.Age(day(6), 90); len(a.Reword) != 0 {
		t.Errorf("the old wording's violation asks to reword the new one: %+v", a.Reword)
	}
	l.Rules[0].LastViolated = "2026-11-06"
	if a := l.Age(day(7), 90); len(a.Reword) != 1 {
		t.Errorf("a violation of the new wording should ask for a reword: %+v", a)
	}

	// A reword the loop proposed matches by rule id even when the words
	// share nothing with the old ones.
	l.Reviews = []Review{{Kind: "reword", Replaces: "r-0002", Rule: "Write judgment, behavior,\n  and color.",
		RuleHash: ruleHash("Write judgment, behavior, and color."), Verdict: "pending"}}
	writeTarget(t, dir, "- Never push without asking first.", "- Write judgment, behavior, and color.")
	res, _ = l.SyncTargets([]string{target, other}, day(8))
	if res != (Sync{Reworded: 1}) || l.Rules[1].ID != "r-0002" || l.Rules[1].Since != "2026-11-08" {
		t.Errorf("proposed reword: %+v %+v", res, l.Rules[1])
	}

	// The same text in another target is the rule moved, not a new one.
	writeTarget(t, dir, "- Never push without asking first.")
	if err := os.WriteFile(other, []byte("- Write judgment, behavior, and color.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ = l.SyncTargets([]string{target, other}, day(9))
	if res != (Sync{Moved: 1}) || l.Rules[1].ID != "r-0002" || l.Rules[1].Target != other || l.Rules[1].Since != "2026-11-08" {
		t.Errorf("moved rule: %+v %+v", res, l.Rules[1])
	}

	// An unrelated replacement is a retirement and a new rule, and a new
	// rule the loop proposed carries the sessions that earned it.
	l.Reviews = append(l.Reviews, Review{Kind: "add", Rule: "Rebase, never merge.",
		RuleHash: ruleHash("Rebase, never merge."), Evidence: []string{"s1", "s2"}, Verdict: "merged"})
	writeTarget(t, dir, "- Rebase, never merge.")
	res, _ = l.SyncTargets([]string{target, other}, day(10))
	if res != (Sync{Added: 1, Removed: 1}) {
		t.Fatalf("replacement: %+v", res)
	}
	added := l.Rules[len(l.Rules)-1]
	if added.ID != "r-0003" || strings.Join(added.SourceSessions, ",") != "s1,s2" {
		t.Errorf("added rule = %+v", added)
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
	}}, nil)
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

// A hand edit is judged from the day it was made, not the day the next sync
// ran: sync stamps a changed rule's Since from the target file's own
// modification time when that is earlier than the sync day, so a session
// between the edit and the sync is still judged against the new wording.
func TestSyncStampsSinceFromHandEditTime(t *testing.T) {
	dir := t.TempDir()
	target := writeTarget(t, dir, "- Never push without asking.")
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	l := &Ledger{}
	if _, err := l.SyncTargets([]string{target}, day(1)); err != nil {
		t.Fatal(err)
	}
	// Edited by hand on the 5th; the next sync runs on the 10th.
	writeTarget(t, dir, "- Never push without asking first.", "- Rebase, never merge.")
	edited := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(target, edited, edited); err != nil {
		t.Fatal(err)
	}
	res, err := l.SyncTargets([]string{target}, day(10))
	if err != nil || res != (Sync{Added: 1, Reworded: 1}) {
		t.Fatalf("sync = %+v, %v", res, err)
	}
	r := l.Rules[0]
	if r.Since != "2026-11-05" || len(r.Supersedes) != 1 || r.Supersedes[0].Until != "2026-11-05" {
		t.Errorf("reworded rule dated by the sync, not the edit: %+v", r)
	}
	if added := l.Rules[1]; added.Since != "2026-11-05" {
		t.Errorf("new bullet dated by the sync, not the edit: %+v", added)
	}
	// A session from the 7th, after the edit and before the sync, is judged
	// against the new wording.
	if got := unjudged(l.Rules, nil, "2026-11-07"); len(got) != 2 {
		t.Errorf("a session between the edit and the sync is judged against %+v, want both rules", got)
	}
	// A file touched after the sync day claims nothing from the future.
	writeTarget(t, dir, "- Never push without asking first.", "- Rebase, never merge.", "- One sentence per line.")
	late := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(target, late, late); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SyncTargets([]string{target}, day(12)); err != nil {
		t.Fatal(err)
	}
	if last := l.Rules[len(l.Rules)-1]; last.Since != "2026-11-12" {
		t.Errorf("a later mtime moved Since forward: %+v", last)
	}
}

// A session whose events carry no readable time is skipped for rule hits:
// nothing says which rules were in force when it ran, so it is judged
// against none rather than against all of them.
func TestUndatedSessionIsJudgedAgainstNoRule(t *testing.T) {
	rules := []Rule{
		{ID: "r-0001", Text: "Predates the ledger."},
		{ID: "r-0002", Text: "Dated.", Since: "2026-01-01"},
	}
	if got := unjudged(rules, nil, ""); len(got) != 0 {
		t.Errorf("an undated session was judged against %+v, want no rules", got)
	}
	if got := unjudged(rules, nil, "2026-02-01"); len(got) != 2 {
		t.Errorf("a session after both rules = %+v, want both", got)
	}
	if got := unjudged(rules, nil, "2025-06-01"); len(got) != 1 || got[0].ID != "r-0001" {
		t.Errorf("a session before r-0002 = %+v, want only the rule that predates the ledger", got)
	}
}

// A ledger date stamped at map time moves back once the session is re-read
// with its own time: ApplyHits recomputes the dates from all hits for the
// rule instead of taking the max with the old value, so `recall observe
// --since all` really is the remedy for a backfilled ledger.
func TestApplyHitsRecomputesDates(t *testing.T) {
	l := &Ledger{Rules: []Rule{
		// Backfill-day dates from an undated record that has since been
		// re-read; the session itself ran in June.
		{ID: "r-0001", Text: "x", LastRelevant: "2026-11-10", LastViolated: "2026-11-10"},
		// No hits in the store say anything about this rule: it keeps its dates.
		{ID: "r-0002", Text: "y", LastRelevant: "2026-03-01"},
	}}
	l.ApplyHits(nil, []SessionHits{{
		LastEvent: "2026-06-02T09:15:00Z",
		Hits:      []RuleHit{{ID: "r-0001", Status: "violated"}},
	}})
	if l.Rules[0].LastRelevant != "2026-06-02" || l.Rules[0].LastViolated != "2026-06-02" {
		t.Errorf("dates = %q, %q, want both moved back to the session's day", l.Rules[0].LastRelevant, l.Rules[0].LastViolated)
	}
	if l.Rules[1].LastRelevant != "2026-03-01" {
		t.Errorf("a rule with no hits lost its dates: %+v", l.Rules[1])
	}
	// The newest hit still wins among the hits given.
	l.ApplyHits(nil, []SessionHits{
		{LastEvent: "2026-06-02T09:15:00Z", Hits: []RuleHit{{ID: "r-0001", Status: "violated"}}},
		{LastEvent: "2026-07-01T09:15:00Z", Hits: []RuleHit{{ID: "r-0001", Status: "followed"}}},
	})
	if l.Rules[0].LastRelevant != "2026-07-01" || l.Rules[0].LastViolated != "2026-06-02" {
		t.Errorf("dates = %q, %q", l.Rules[0].LastRelevant, l.Rules[0].LastViolated)
	}
}

// A session mapped today is dated by when it ran: a backfill of old
// sessions moves no rule's dates to the day of the backfill.
func TestHitsAndEvidenceUseSessionTime(t *testing.T) {
	st := store.NewMem()
	fake := &model.Fake{Response: "Observation[context]: Repos live under a GOPATH-style tree.\nRule[r-0001]: followed\n"}
	rules := []Rule{{ID: "r-0001", Text: "Never push without asking.", Created: "2026-01-01"}}
	m := mapper(st, fake, rules) // the fake clock maps on 2026-11-11
	evs := []events.Event{
		{Kind: events.UserPrompt, Text: "clone it", Timestamp: "2026-06-01T23:30:00-07:00"},
		{Kind: events.AssistantText, Text: "done", Timestamp: "2026-06-02T09:15:00Z"},
		{Kind: events.AssistantText, Text: "no timestamp"},
	}
	res, err := m.MapSession(context.Background(), "claude", "s1", evs)
	if err != nil {
		t.Fatal(err)
	}
	so := res.Observations
	if so.FirstEvent != "2026-06-02T06:30:00Z" || so.LastEvent != "2026-06-02T09:15:00Z" {
		t.Errorf("span = %q to %q", so.FirstEvent, so.LastEvent)
	}
	if !strings.HasPrefix(so.ObservedAt, "2026-11-11") || so.Day() != "2026-06-02" {
		t.Errorf("observed %q, day %q; want the map day kept and the session day used", so.ObservedAt, so.Day())
	}
	l := &Ledger{Rules: rules}
	l.ApplyHits(nil, []SessionHits{*res.Hits})
	if l.Rules[0].LastRelevant != "2026-06-02" {
		t.Errorf("last relevant = %q, want the session's day, not the map's", l.Rules[0].LastRelevant)
	}
	r := &Reducer{Ledger: l, Clock: m.Clock, RetireDays: 365}
	if in := r.BuildInput([]SessionObservations{*so}); !strings.Contains(in, "2026-06-02 session s1:") {
		t.Errorf("reduce input doesn't date the observation by its session:\n%s", in)
	}

	// A record from before session dating has only its map time.
	legacy := SessionObservations{ObservedAt: "2026-11-05T10:00:00Z"}
	if legacy.Dated() || legacy.Day() != "2026-11-05" {
		t.Errorf("legacy record: dated=%v day=%q", legacy.Dated(), legacy.Day())
	}
}

// With archive-chats.redact on, observation text and quotes pass the
// redactor before they are written, and no observation record holds the
// secret in the clear.
func TestLearnStoreRedacted(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()

	secret := "ghp_" + strings.Repeat("A1b2C3", 6) // a GitHub-shaped token
	fake := &model.Fake{Response: "Observation[tool-failure]: The push failed with " + secret + " in the remote URL.\n" +
		"Rule[r-0001]: violated\n"}
	rules := []Rule{{ID: "r-0001", Text: "Never paste a token.", TextHash: textHash("Never paste a token.")}}
	m := mapper(st, fake, rules)
	m.Redact = true
	evs := []events.Event{ev(events.UserPrompt, "push it to the fork")}
	if _, err := m.MapSession(ctx, "claude", "s1", evs); err != nil {
		t.Fatal(err)
	}

	// No object under analysis/ holds the planted secret in the clear.
	keys, err := st.List(ctx, "analysis/")
	if err != nil || len(keys) < 2 {
		t.Fatalf("keys = %v, %v; want observations and hits", keys, err)
	}
	for _, k := range keys {
		r, err := st.Get(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(r)
		r.Close()
		if bytes.Contains(raw, []byte("ghp_")) {
			t.Errorf("%s holds the secret in the clear", k)
		}
	}

	// The cache answers without another model call, and the window holds the
	// redacted text.
	if res, err := m.MapSession(ctx, "claude", "s1", evs); err != nil || res.Ran() || len(fake.Calls) != 1 {
		t.Errorf("cached read: res=%+v err=%v calls=%d", res, err, len(fake.Calls))
	}
	window, err := ListObservations(ctx, st, "claude")
	if err != nil || len(window) != 1 {
		t.Fatalf("window = %+v, %v", window, err)
	}
	text := window[0].Observations[0].Text
	if strings.Contains(text, "ghp_") || !strings.Contains(text, "[REDACTED:github-token]") {
		t.Errorf("observation text not redacted: %q", text)
	}
}

// A record written by the removed client-side encryption feature is an
// error that names the object, never a silent skip.
func TestEncryptedRecordIsNamedNotSkipped(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	key := Key("claude", "s0", "deadbeef") + ".age"
	if err := st.PutIfAbsent(ctx, key, strings.NewReader("age-encryption.org/v1\n")); err != nil {
		t.Fatal(err)
	}
	_, err := ListObservations(ctx, st, "claude")
	if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("err = %v, want it to name %s and say the feature was removed", err, key)
	}
}

func TestReducerBuildsInputAndWritesProposals(t *testing.T) {
	ledger := &Ledger{Rules: []Rule{
		{ID: "r-0001", Target: "/home/you/.claude/CLAUDE.md", Text: "Never push without asking.",
			Created: "2026-01-01", LastRelevant: "2026-11-01"},
		{ID: "r-0002", Target: "/home/you/.claude/CLAUDE.md", Text: "stale rule",
			Created: "2026-01-01", LastRelevant: "2026-02-01"},
	}, Reviews: []Review{
		{Kind: "add", Title: "Waiting", Rule: "Wait\n  for it.", Proposed: "2026-11-04", Verdict: "pending"},
		{Kind: "skill", Title: "Said no", Rule: "x", Verdict: "closed", Decided: "2026-11-06"},
		{Kind: "add", Title: "Long ago", Rule: "y", Verdict: "closed", Decided: "2026-01-01"},
		{Kind: "add", Title: "Landed", Rule: "z", Verdict: "merged", Decided: "2026-11-06"},
	}}
	window := []SessionObservations{{
		Session: "s1", ObservedAt: "2026-11-05T00:00:00Z",
		Observations: []Observation{{Category: "correction", Text: "Quote PRs in full.", Quote: "quote it", Source: SourceOwner}},
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
		"Candidate[c-0001] (1 session(s); 2026-11-05)",
		`- [correction, owner-typed] 2026-11-05 session s1: Quote PRs in full. Owner: "quote it"`,
		"retire Rule[r-0002]",
		"# REVIEWS", "- pending since 2026-11-04: add: Waiting: Wait for it.",
		"- rejected 2026-11-06: skill: Said no: x",
	} {
		if !strings.Contains(input, want) {
			t.Errorf("reduce input misses %q", want)
		}
	}
	for _, gone := range []string{"Long ago", "Landed"} {
		if strings.Contains(input, gone) {
			t.Errorf("reduce input shows %q, an old rejection or a merged review", gone)
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

var refuserUsage = model.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.01, Credits: 0.5}

func (r *refuser) Run(ctx context.Context, _ model.Call, input string) (string, model.Usage, error) {
	r.calls++
	if strings.Contains(input, "FLAGGED") {
		return "", refuserUsage, fmt.Errorf("claude -p: %w: flagged", model.ErrRefusal)
	}
	return "Observation[context]: saw " + fmt.Sprint(strings.Count(input, "[owner] ")) + " prompts\n", refuserUsage, nil
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
			res, err := m.MapSession(context.Background(), "claude", "s1", evs)
			if err != nil || !res.Observed {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			so := res.Observations
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
			if so.InputTokens != 100*tc.wantCalls || so.OutputTokens != 10*tc.wantCalls || so.Credits != 0.5*float64(tc.wantCalls) {
				t.Errorf("spend = %+v, want every attempt's tokens and credits counted", so.Spend)
			}
			if res, _ := m.MapSession(context.Background(), "claude", "s1", evs); res.Ran() {
				t.Error("a salvaged session should be cached")
			}
		})
	}
}

// Tokens and credits land in a session's records beside its cost.
func TestMapRecordsSessionSpend(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	fake := &model.Fake{Response: "Observation[context]: Repos live under a GOPATH-style tree.\n",
		Usage: model.Usage{InputTokens: 1000, OutputTokens: 50, Credits: 0.25}}
	m := mapper(st, fake, nil)
	evs := []events.Event{ev(events.UserPrompt, "ship it")}
	res, err := m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || !res.Observed {
		t.Fatalf("map: err=%v res=%+v", err, res)
	}
	want := Spend{InputTokens: 1000, OutputTokens: 50, Credits: 0.25}
	if res.Observations.Spend != want || res.Usage.InputTokens != 1000 {
		t.Errorf("observation spend = %+v, usage = %+v", res.Observations.Spend, res.Usage)
	}
	m.Rules = []Rule{{ID: "r-0001", Text: "Ask first.", TextHash: textHash("Ask first."), Created: "2026-01-01"}}
	fake.Response = "Rule[r-0001]: followed\n"
	res, err = m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || res.Observed || !res.Judged || res.Hits.Spend != want {
		t.Fatalf("judge: err=%v res=%+v", err, res)
	}
	keys, err := st.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	stored := 0
	for _, k := range keys {
		r, err := st.Get(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(r)
		r.Close()
		for _, field := range []string{`"input_tokens": 1000`, `"output_tokens": 50`, `"credits": 0.25`} {
			if !strings.Contains(string(data), field) {
				t.Errorf("%s misses %s:\n%s", k, field, data)
			}
		}
		if strings.Contains(string(data), "cost_usd") {
			t.Errorf("%s records a cost the runner didn't report", k)
		}
		stored++
	}
	if stored != 2 {
		t.Errorf("stored %d records, want an observation record and a hits record", stored)
	}

	old := `{"session":"s0","cost_usd":0.02,"observations":[]}`
	var so SessionObservations
	var sh SessionHits
	if err := json.Unmarshal([]byte(old), &so); err != nil || so.Spend != (Spend{CostUSD: 0.02}) {
		t.Errorf("old observation record = %+v, %v", so.Spend, err)
	}
	if err := json.Unmarshal([]byte(old), &sh); err != nil || sh.Spend != (Spend{CostUSD: 0.02}) {
		t.Errorf("old hits record = %+v, %v", sh.Spend, err)
	}
}

// A refused judgment of an already-observed session is recorded, so later
// runs don't pay to be refused again.
func TestRefusedJudgmentIsCached(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	m := mapper(st, &model.Fake{Response: "NONE\n"}, nil)
	evs := []events.Event{ev(events.UserPrompt, "do the thing")}
	if _, err := m.MapSession(ctx, "claude", "s1", evs); err != nil {
		t.Fatal(err)
	}
	refuse := &model.Fake{Err: fmt.Errorf("claude -p: %w: declined", model.ErrRefusal)}
	m = mapper(st, refuse, []Rule{{ID: "r-0001", Text: "Ask first.", TextHash: textHash("Ask first.")}})
	res, err := m.MapSession(ctx, "claude", "s1", evs)
	if err != nil || res.Observed || !res.Judged || !res.Hits.Refused || len(res.Hits.Hits) != 0 {
		t.Fatalf("refused judgment: err=%v res=%+v", err, res)
	}
	if res, _ := m.MapSession(ctx, "claude", "s1", evs); res.Ran() || len(refuse.Calls) != 1 {
		t.Errorf("a refused judgment was tried again: calls=%d", len(refuse.Calls))
	}
}

func TestWouldCall(t *testing.T) {
	ctx := context.Background()
	m := mapper(store.NewMem(), &model.Fake{Response: "NONE\n"}, nil)
	evs := []events.Event{ev(events.UserPrompt, "do the thing")}
	if observe, _, err := m.WouldCall(ctx, "claude", "s1", evs); err != nil || !observe {
		t.Fatalf("new session: observe=%v err=%v", observe, err)
	}
	if _, err := m.MapSession(ctx, "claude", "s1", evs); err != nil {
		t.Fatal(err)
	}
	if observe, judge, _ := m.WouldCall(ctx, "claude", "s1", evs); observe || judge {
		t.Error("a cached session would call the model again")
	}
	if observe, judge, _ := m.WouldCall(ctx, "claude", "s2", []events.Event{ev(events.AssistantText, "hi")}); observe || judge {
		t.Error("a session with no owner prompts would call the model")
	}
}

// WouldSend sizes the very input the call then sends.
func TestWouldSendSizesTheInput(t *testing.T) {
	ctx := context.Background()
	f := &model.Fake{Response: "NONE\n"}
	m := mapper(store.NewMem(), f, nil)
	evs := []events.Event{ev(events.UserPrompt, "fais la tâche, s'il te plaît")}
	_, _, chars, err := m.WouldSend(ctx, "claude", "s1", evs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.MapSession(ctx, "claude", "s1", evs); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 || chars != utf8.RuneCountInString(f.Calls[0].Input) || chars == len(f.Calls[0].Input) {
		t.Errorf("WouldSend = %d chars, but the call sent %d (%d bytes)", chars, utf8.RuneCountInString(f.Calls[0].Input), len(f.Calls[0].Input))
	}
	if _, _, chars, _ := m.WouldSend(ctx, "claude", "s1", evs); chars != 0 {
		t.Errorf("a cached session would send %d chars", chars)
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

func TestReduceOwnerInstructions(t *testing.T) {
	r := &Reducer{Ledger: &Ledger{}, Clock: &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}}
	if strings.Contains(r.BuildInput(nil), "# OWNER INSTRUCTIONS") {
		t.Error("no instructions should add no section")
	}
	r.OwnerInstructions = "- Prefer rewording to adding.\n"
	in := r.BuildInput(nil)
	if !strings.Contains(in, "# OWNER INSTRUCTIONS\n\n- Prefer rewording to adding.\n") {
		t.Errorf("input misses the owner's instructions:\n%s", in)
	}
}
