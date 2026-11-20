package learn

import (
	"context"
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

const outgrownRule = "Always quote words that begin with an equals sign."

// outgrownLedger is a rule earned on a model no recent session runs, with
// the session that earned it, and a window where only a newer model answers
// now.
func outgrownLedger(target string) (*Ledger, []SessionObservations) {
	ledger := &Ledger{Rules: []Rule{{
		ID: "r-0001", Target: target, Text: outgrownRule, TextHash: textHash(outgrownRule),
		Created: "2026-05-01", LastRelevant: "2026-11-01",
		Models: []string{"claude-old-1"}, SourceSessions: []string{"s1"},
	}}}
	s1 := session("s1", "2026-10-01", "Words that begin with an equals sign must be quoted in shell commands.")
	s1.Models = []string{"claude-old-1"}
	s1.Observations[0].Quote = "quote the equals sign"
	s1.Observations[0].Source = SourceOwner
	s2 := session("s2", "2026-11-10", "Pull request descriptions stay short and high level.")
	s2.Models = []string{"claude-new-1"}
	return ledger, []SessionObservations{s1, s2}
}

func loadOutgrownSession(harness, id string) ([]events.Event, bool) {
	if harness != "claude" || id != "s1" {
		return nil, false
	}
	return []events.Event{
		{Kind: events.UserPrompt, Text: "set the env var in the deploy script"},
		{Kind: events.AssistantText, Text: "I wrote the assignment unquoted."},
		{Kind: events.UserPrompt, Text: "please quote the equals sign next time"},
	}, true
}

// Phase 4 gate: replay retires one rule the current model follows
// unprompted. The rule was earned on a model no recent session runs, so the
// ablation replays its earning moment on the current model with the rule
// removed from its file; the agent avoids the mistake, the binary proposes
// the retire citing the replay, and the apply lands it on a branch for
// review, through the whole loop on fakes.
func TestPhase4GateReplayRetiresRuleCurrentModelFollowsUnprompted(t *testing.T) {
	ctx := context.Background()
	isolateGit(t)
	base := review.Resolve(t.TempDir())
	repo := liveRepo(t, base, "home", map[string]string{"CLAUDE.md": "# Rules\n\n- " + outgrownRule + "\n"})
	target := filepath.Join(repo, "CLAUDE.md")
	ledger, window := outgrownLedger(target)

	fake := &model.Fake{Responses: []string{
		"## Proposals\n\n(none)\n",
		"Next: write the assignment quoted\nVerdict: avoids",
	}}
	fakeClock := &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	r := &Reducer{Runner: fake, Model: "opus", Ledger: ledger, Home: base, Targets: []string{target},
		Clock:  fakeClock,
		Replay: &Replayer{Runner: fake, Model: "sonnet", Load: loadOutgrownSession}}
	dir := filepath.Join(base, "state", "2026-11-11")
	path, _, err := r.Run(ctx, window, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("calls = %d, want the reduce and one ablation replay", len(fake.Calls))
	}
	in := fake.Calls[1].Input
	if strings.Contains(in, "- "+outgrownRule) {
		t.Errorf("the ablation replay still shows the rule:\n%s", in)
	}
	if !strings.Contains(in, "[owner] set the env var in the deploy script") || !strings.Contains(in, "# Rules") {
		t.Errorf("the ablation replay misses the prefix or the file without the rule:\n%s", in)
	}
	md, _ := os.ReadFile(path)
	proposals := string(md)
	title := "Retire r-0001: the current model follows it unprompted"
	for _, want := range []string{"### retire: " + title, "- replaces: r-0001", "- target: " + target, "claude-old-1", "s1"} {
		if !strings.Contains(proposals, want) {
			t.Errorf("proposals miss %q:\n%s", want, proposals)
		}
	}
	recs, err := LoadReplays(dir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("replays = %+v, %v", recs, err)
	}
	if recs[0].Kind != "retire" || recs[0].Without != "avoids" || recs[0].Held != "" || recs[0].Session != "s1" {
		t.Errorf("record = %+v", recs[0])
	}

	editor := &model.FakeEditor{Apply: func(input string, dirs []string) (string, error) {
		var at string
		for _, line := range strings.Split(input, "\n") {
			if p, ok := strings.CutPrefix(line, "- edit: "); ok {
				at = p
			}
		}
		data, err := os.ReadFile(at)
		if err != nil {
			return "", err
		}
		kept := strings.ReplaceAll(string(data), "- "+outgrownRule+"\n", "")
		if err := os.WriteFile(at, []byte(kept), 0o644); err != nil {
			return "", err
		}
		return "Applied[retire]: " + title + " -- " + at, nil
	}}
	a := &Applier{Editor: editor, Model: "sonnet", Publisher: review.Branch{},
		Targets: []string{target}, Home: base, Clock: fakeClock, Ledger: ledger}
	plan, err := a.Plan(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 1 || plan.Routes[0].Skip != "" || plan.Routes[0].Kind != "retire" {
		t.Fatalf("routes = %+v, want the one retire going ahead", plan.Routes)
	}
	res, err := a.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 || res.Changes[0].Error != "" {
		t.Fatalf("changes = %+v", res.Changes)
	}
	// The branch commit removes the rule; the live file waits for review.
	if got := gitIn(t, repo, "show", plan.Branch+":CLAUDE.md"); strings.Contains(got, outgrownRule) {
		t.Errorf("the branch still holds the retired rule: %q", got)
	}
	if live, _ := os.ReadFile(target); !strings.Contains(string(live), outgrownRule) {
		t.Errorf("the live file changed before review: %q", live)
	}
}

// The ablation is part of learn.replay: with it off, an outgrown rule gets
// no retire and no replay runs.
func TestAblationRunsOnlyWithReplayOn(t *testing.T) {
	base := review.Resolve(t.TempDir())
	target := filepath.Join(base, "CLAUDE.md")
	writeFile(t, target, "- "+outgrownRule+"\n")
	ledger, window := outgrownLedger(target)
	fake := &model.Fake{Responses: []string{"## Proposals\n\n(none)\n"}}
	r := &Reducer{Runner: fake, Model: "opus", Ledger: ledger, Home: base, Targets: []string{target},
		Clock: &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}}
	path, _, err := r.Run(context.Background(), window, filepath.Join(base, "state", "2026-11-11"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 1 {
		t.Errorf("calls = %d, want only the reduce", len(fake.Calls))
	}
	if md, _ := os.ReadFile(path); strings.Contains(string(md), "### retire:") {
		t.Errorf("a retire was proposed with replay off:\n%s", md)
	}
}

// A rule whose moment the current model still gets wrong is not retired:
// one repeats verdict, and the ablation proposes nothing.
func TestAblationKeepsARuleStillNeeded(t *testing.T) {
	base := review.Resolve(t.TempDir())
	target := filepath.Join(base, "CLAUDE.md")
	writeFile(t, target, "- "+outgrownRule+"\n")
	ledger, window := outgrownLedger(target)
	fake := &model.Fake{Responses: []string{
		"## Proposals\n\n(none)\n",
		"Next: write the assignment unquoted\nVerdict: repeats",
	}}
	r := &Reducer{Runner: fake, Model: "opus", Ledger: ledger, Home: base, Targets: []string{target},
		Clock:  &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)},
		Replay: &Replayer{Runner: fake, Model: "sonnet", Load: loadOutgrownSession}}
	dir := filepath.Join(base, "state", "2026-11-11")
	path, _, err := r.Run(context.Background(), window, dir)
	if err != nil {
		t.Fatal(err)
	}
	if md, _ := os.ReadFile(path); strings.Contains(string(md), "### retire:") {
		t.Errorf("a retire was proposed though the replay repeated the mistake:\n%s", md)
	}
	recs, _ := LoadReplays(dir)
	if len(recs) != 1 || recs[0].Without != "repeats" || recs[0].Held != "" {
		t.Errorf("records = %+v", recs)
	}
}
