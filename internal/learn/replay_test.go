package learn

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/review"
)

// replaySession is a transcript with a decision point: the owner asks, the
// agent answers wrongly, and the owner corrects it with the quoted words.
func replaySession() []events.Event {
	return []events.Event{
		{Kind: events.UserPrompt, Text: "please format the sample config file"},
		{Kind: events.AssistantText, Text: "I reformatted it with spaces."},
		{Kind: events.UserPrompt, Text: "no, use tabs not spaces in that file"},
	}
}

// replayWindow is one mapped session whose correction carries an
// owner-typed quote anchored in replaySession.
func replayWindow() []SessionObservations {
	so := session("s1", "2026-11-05", "Config files are formatted with tabs, not spaces.")
	so.Prompts = 2
	so.Observations[0].Quote = "use tabs not spaces"
	so.Observations[0].Source = SourceOwner
	return []SessionObservations{so}
}

func loadReplaySession(harness, sessionID string) ([]events.Event, bool) {
	if harness == "claude" && sessionID == "s1" {
		return replaySession(), true
	}
	return nil, false
}

func addProposal(target string) string {
	return "## Proposals\n\n### add: Tabs in configs\n- target: " + target +
		"\n- rule: Use tabs, not spaces, in config files.\n- evidence: c-0001\n- why: x\n"
}

// replayReducer wires a Reducer whose fake runner answers the reduce first
// and each replay arm after, and returns it with the run directory.
func replayReducer(t *testing.T, base, target string, fake *model.Fake) (*Reducer, string) {
	t.Helper()
	r := &Reducer{Runner: fake, Model: "opus", Effort: "max", Ledger: &Ledger{}, Home: base,
		Targets: []string{target},
		Clock:   &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)},
		Replay:  &Replayer{Runner: fake, Model: "sonnet", Effort: "high", Load: loadReplaySession},
	}
	return r, filepath.Join(base, "state", "2026-11-11")
}

// A replay that ends the same way with and without the rule holds the add
// back, in the proposal output and in the apply's plan.
func TestReplayHoldsBackAddWithNoDifference(t *testing.T) {
	base := review.Resolve(t.TempDir())
	target := filepath.Join(base, "CLAUDE.md")
	writeFile(t, target, "- Existing rule.\n")
	fake := &model.Fake{Responses: []string{
		addProposal(target),
		"Next: reformat the file with spaces\nVerdict: avoids",
		"Next: reformat the file with spaces\nVerdict: avoids",
	}, Usage: model.Usage{ModelID: "claude-full-id"}}
	r, dir := replayReducer(t, base, target, fake)
	path, _, err := r.Run(context.Background(), replayWindow(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 3 {
		t.Fatalf("calls = %d, want the reduce and the replay's two arms", len(fake.Calls))
	}
	var run ReduceRecord
	if data, err := os.ReadFile(filepath.Join(dir, reduceFile)); err != nil || json.Unmarshal(data, &run) != nil ||
		run != (ReduceRecord{Model: "opus", Effort: "max", ModelID: "claude-full-id",
			ReplayModel: "sonnet", ReplayEffort: "high", ReplayModelID: "claude-full-id"}) {
		t.Errorf("reduce.json = %+v (%v)", run, err)
	}
	if recs, err := LoadReplays(dir); err != nil || len(recs) != 1 || recs[0].ModelID != "claude-full-id" {
		t.Errorf("replay records = %+v (%v), want the resolved model ID", recs, err)
	}
	if call := fake.Calls[0]; call.Model != "opus" || call.Effort != "max" {
		t.Errorf("reduce ran on %q at %q, want the reducer's model and effort", call.Model, call.Effort)
	}
	for i, call := range fake.Calls[1:] {
		if call.Model != "sonnet" || call.Effort != "high" {
			t.Errorf("replay arm %d ran on %q at %q, want the replayer's model and effort", i, call.Model, call.Effort)
		}
		if !strings.Contains(call.Input, "[owner] please format the sample config file") {
			t.Errorf("replay arm %d misses the prefix prompt:\n%s", i, call.Input)
		}
		if strings.Contains(call.Input, "# SESSION\n\n[owner] no, use tabs") {
			t.Errorf("replay arm %d replays past the correction:\n%s", i, call.Input)
		}
		if !strings.Contains(call.Input, "Existing rule.") {
			t.Errorf("replay arm %d misses the target file's rules:\n%s", i, call.Input)
		}
	}
	if in := fake.Calls[1].Input; strings.Contains(in, "Use tabs, not spaces, in config files.") {
		t.Errorf("the without arm already holds the proposed rule:\n%s", in)
	}
	if in := fake.Calls[2].Input; !strings.Contains(in, "- Use tabs, not spaces, in config files.") {
		t.Errorf("the with arm misses the proposed rule:\n%s", in)
	}
	md, _ := os.ReadFile(path)
	if !strings.Contains(string(md), "## Replay") || !strings.Contains(string(md), "no difference") {
		t.Errorf("proposals should hold the replay's reason:\n%s", md)
	}
	recs, err := LoadReplays(dir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("replays = %+v, %v", recs, err)
	}
	if recs[0].Without != "avoids" || recs[0].With != "avoids" || recs[0].Held == "" || recs[0].Session != "s1" {
		t.Errorf("record = %+v", recs[0])
	}
	gitIn(t, base, "init", "-q")
	a := &Applier{Publisher: &review.Fake{}, Targets: []string{target}, Home: base,
		Clock: &clock.Fake{T: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}}
	plan, err := a.Plan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 1 || !strings.Contains(plan.Routes[0].Skip, "no difference") {
		t.Errorf("routes = %+v, want the add held back by its replay", plan.Routes)
	}
}

// A replay where the rule turns the mistake into avoidance holds nothing
// back.
func TestReplayDifferenceLetsAddThrough(t *testing.T) {
	base := review.Resolve(t.TempDir())
	target := filepath.Join(base, "CLAUDE.md")
	writeFile(t, target, "- Existing rule.\n")
	fake := &model.Fake{Responses: []string{
		addProposal(target),
		"Next: reformat the file with spaces\nVerdict: repeats",
		"Next: reformat the file with tabs\nVerdict: avoids",
	}}
	r, dir := replayReducer(t, base, target, fake)
	path, _, err := r.Run(context.Background(), replayWindow(), dir)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(path)
	if strings.Contains(string(md), "## Replay") {
		t.Errorf("nothing should be held back:\n%s", md)
	}
	recs, _ := LoadReplays(dir)
	if len(recs) != 1 || recs[0].Held != "" || recs[0].Without != "repeats" || recs[0].With != "avoids" {
		t.Errorf("record = %+v", recs)
	}
	gitIn(t, base, "init", "-q")
	a := &Applier{Publisher: &review.Fake{}, Targets: []string{target}, Home: base,
		Clock: &clock.Fake{T: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}}
	plan, err := a.Plan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 1 || plan.Routes[0].Skip != "" {
		t.Errorf("routes = %+v, want the add going ahead", plan.Routes)
	}
}

// A replay that fails, here with output in no verdict shape, is recorded
// and blocks nothing.
func TestReplayFailureDoesNotBlock(t *testing.T) {
	base := review.Resolve(t.TempDir())
	target := filepath.Join(base, "CLAUDE.md")
	writeFile(t, target, "- Existing rule.\n")
	fake := &model.Fake{Responses: []string{
		addProposal(target),
		"I would probably reformat the file, hard to say how.",
	}}
	r, dir := replayReducer(t, base, target, fake)
	path, _, err := r.Run(context.Background(), replayWindow(), dir)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(path)
	if strings.Contains(string(md), "## Replay") {
		t.Errorf("a failed replay must hold nothing back:\n%s", md)
	}
	recs, _ := LoadReplays(dir)
	if len(recs) != 1 || recs[0].Held != "" || !strings.Contains(recs[0].Error, "no Verdict line") {
		t.Errorf("record = %+v", recs)
	}
	gitIn(t, base, "init", "-q")
	a := &Applier{Publisher: &review.Fake{}, Targets: []string{target}, Home: base,
		Clock: &clock.Fake{T: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}}
	plan, err := a.Plan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 1 || plan.Routes[0].Skip != "" {
		t.Errorf("routes = %+v, want the add going ahead after a failed replay", plan.Routes)
	}
}

// An add with no owner-typed quote anchored in a session still here has no
// moment to replay, which is a logged failure, never a holdback.
func TestReplayNeedsAMomentOnThisMachine(t *testing.T) {
	base := review.Resolve(t.TempDir())
	target := filepath.Join(base, "CLAUDE.md")
	writeFile(t, target, "- Existing rule.\n")
	fake := &model.Fake{Responses: []string{addProposal(target)}}
	r, dir := replayReducer(t, base, target, fake)
	r.Replay.Load = func(string, string) ([]events.Event, bool) { return nil, false }
	if _, _, err := r.Run(context.Background(), replayWindow(), dir); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("calls = %d, want only the reduce", len(fake.Calls))
	}
	recs, _ := LoadReplays(dir)
	if len(recs) != 1 || recs[0].Held != "" || !strings.Contains(recs[0].Error, "no replayable moment") {
		t.Errorf("record = %+v", recs)
	}
}

// The per-run cap bounds what a run spends: a moment past it isn't
// replayed, and its proposal goes ahead.
func TestReplayCapBoundsTheRun(t *testing.T) {
	base := review.Resolve(t.TempDir())
	target := filepath.Join(base, "CLAUDE.md")
	writeFile(t, target, "- Existing rule.\n")
	second := session("s2", "2026-11-06", "Pull request descriptions stay short and high level.")
	second.Observations[0].Quote = "keep the description short"
	second.Observations[0].Source = SourceOwner
	window := append(replayWindow(), second)
	load := func(harness, id string) ([]events.Event, bool) {
		if id == "s2" {
			return []events.Event{
				{Kind: events.UserPrompt, Text: "open a pull request for this change"},
				{Kind: events.AssistantText, Text: "I wrote a ten-paragraph description."},
				{Kind: events.UserPrompt, Text: "keep the description short next time"},
			}, true
		}
		return loadReplaySession(harness, id)
	}
	fake := &model.Fake{Responses: []string{
		addProposal(target) +
			"\n### add: Short descriptions\n- target: " + target +
			"\n- rule: Keep pull request descriptions short.\n- evidence: c-0002\n- why: x\n",
		"Next: reformat with spaces\nVerdict: avoids",
		"Next: reformat with spaces\nVerdict: avoids",
	}}
	r, dir := replayReducer(t, base, target, fake)
	r.Replay.Load = load
	r.Replay.Cap = 1
	if _, _, err := r.Run(context.Background(), window, dir); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 3 {
		t.Fatalf("calls = %d, want the reduce and one replay's two arms", len(fake.Calls))
	}
	recs, _ := LoadReplays(dir)
	if len(recs) != 2 {
		t.Fatalf("records = %+v", recs)
	}
	if recs[0].Held == "" {
		t.Errorf("the first add's replay showed no difference: %+v", recs[0])
	}
	if recs[1].Held != "" || !strings.Contains(recs[1].Error, "replay cap reached") {
		t.Errorf("the capped add must go ahead unreplayed: %+v", recs[1])
	}
}

// With learn.replay off the reducer has no Replayer, calls no replay, and
// writes no replay record.
func TestReplayOffByDefault(t *testing.T) {
	base := review.Resolve(t.TempDir())
	target := filepath.Join(base, "CLAUDE.md")
	writeFile(t, target, "- Existing rule.\n")
	fake := &model.Fake{Responses: []string{addProposal(target)}}
	r, dir := replayReducer(t, base, target, fake)
	r.Replay = nil
	if _, _, err := r.Run(context.Background(), replayWindow(), dir); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 1 {
		t.Errorf("calls = %d, want only the reduce", len(fake.Calls))
	}
	if _, err := os.Stat(filepath.Join(dir, replayFile)); !os.IsNotExist(err) {
		t.Errorf("replay.json written with replay off: %v", err)
	}
}
