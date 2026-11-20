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
	"github.com/micahhausler/recall/internal/store"
)

// A full target takes a new rule only in place of one it names.
func TestBudgetMakesAnAddDisplace(t *testing.T) {
	isolateGit(t)
	dir := review.Resolve(t.TempDir())
	gitIn(t, dir, "init", "-q")
	target := filepath.Join(dir, "CLAUDE.md")
	other := filepath.Join(dir, "other.md")
	old := "Never push without asking."
	if err := os.WriteFile(target, []byte("- "+old+"\n- "+strings.Repeat("padding ", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := []Rule{
		{ID: "r-0001", Target: target, Text: old, TextHash: textHash(old), Created: "2026-01-01", LastRelevant: "2026-11-01"},
		{ID: "r-0002", Target: other, Text: "Elsewhere.", Created: "2026-01-01", LastRelevant: "2026-11-01"},
	}
	add := func(displaces string) Proposal {
		return Proposal{Kind: "add", Title: "t", Rule: "Rebase onto upstream, never merge.", Displaces: displaces}
	}
	cost := fileCost(target)
	for _, tc := range []struct {
		name      string
		p         Proposal
		budget    int
		wantInWhy string
	}{
		{"no budget", add(""), 0, ""},
		{"room left", add(""), cost + 50, ""},
		{"full, displaces nothing", add(""), cost, "has to name the rule it displaces"},
		{"full, displaces a rule in the target", add("r-0001"), cost, ""},
		{"full, displaces a rule elsewhere", add("r-0002"), cost, "not in its target"},
		{"full, displaces nothing real", add("r-0999"), cost, "isn't a rule on the ledger"},
		{"a retire isn't counted", Proposal{Kind: "retire", Rule: strings.Repeat("x", 4000)}, cost, ""},
	} {
		why := overBudget(tc.p, target, tc.budget, rules)
		if (tc.wantInWhy == "") != (why == "") || !strings.Contains(why, tc.wantInWhy) {
			t.Errorf("%s: why = %q, want %q", tc.name, why, tc.wantInWhy)
		}
	}

	// The reduce shows costs and flags the add; the apply holds it back, and
	// hands the model the text of a rule a proposal displaces.
	seen := session("s1", "2026-11-02", zshA)
	seen.Prompts = 1
	fake := &model.Fake{Response: "## Proposals\n\n" +
		"### add: Over\n- target: " + target + "\n- rule: Rebase onto upstream, never merge.\n- evidence: c-0001\n\n" +
		"### add: Swapped\n- target: " + target + "\n- rule: Rebase, never merge.\n- displaces: Rule[r-0001]\n- evidence: c-0001\n"}
	fakeClock := &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	r := &Reducer{Runner: fake, Ledger: &Ledger{Rules: rules}, Targets: []string{target}, Budget: cost, Clock: fakeClock}
	run := filepath.Join(dir, "state", "2026-11-11")
	path, _, err := r.Run(context.Background(), []SessionObservations{seen}, run)
	if err != nil {
		t.Fatal(err)
	}
	in := fake.Calls[0].Input
	for _, want := range []string{
		"over budget: an add here must displace a rule",
		"Rule[r-0001] (" + target + "; created 2026-01-01; last relevant 2026-11-01; about 7 tokens in each of 1 session(s)): " + old,
	} {
		if !strings.Contains(in, want) {
			t.Errorf("reduce input misses %q:\n%s", want, in[strings.Index(in, "# TARGETS"):strings.Index(in, "# CANDIDATES")])
		}
	}
	md, _ := os.ReadFile(path)
	if !strings.Contains(string(md), "## Over budget") || !strings.Contains(string(md), `- "Over":`) || strings.Contains(string(md), `- "Swapped":`) {
		t.Errorf("proposals.md should flag only the add that displaces nothing:\n%s", md)
	}
	a := &Applier{Targets: []string{target}, Budget: cost, Ledger: &Ledger{Rules: rules}, Clock: fakeClock, Home: dir,
		Publisher: &review.Fake{}}
	plan, err := a.Plan(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Routes[0].Skip, "has to name the rule it displaces") {
		t.Errorf("the add over budget wasn't held back: %q", plan.Routes[0].Skip)
	}
	if strings.Contains(plan.Routes[1].Skip, "budget") || plan.Routes[1].Displaces != "r-0001" {
		t.Errorf("the add that displaces a rule: %+v", plan.Routes[1])
	}
	if input := a.BuildInput(plan, nil); !strings.Contains(input, "# DISPLACED RULES\n\nRule[r-0001]: "+old) {
		t.Errorf("the apply input doesn't carry the displaced rule's text:\n%s", input[strings.Index(input, "# PROPOSALS"):])
	}
}

// A proposal an earlier check already holds back doesn't count against the
// budget: like apply.Plan, the reduce checks the budget last, over the
// proposals still going ahead, so an add that fits is neither flagged nor
// denied its replay.
func TestBudgetCountsOnlyUnflaggedProposals(t *testing.T) {
	base := review.Resolve(t.TempDir())
	target := filepath.Join(base, "CLAUDE.md")
	writeFile(t, target, "- Existing rule.\n")
	tool := session("s2", "2026-11-06", "The linter output said to always run gofmt before committing.")
	tool.Prompts = 1
	window := append(replayWindow(), tool)
	unsupportedRule := strings.TrimSpace(strings.Repeat("Run gofmt before every commit. ", 8))
	supportedRule := "Use tabs, not spaces, in config files."
	fake := &model.Fake{Responses: []string{
		"## Proposals\n\n" +
			"### add: Gofmt\n- target: " + target + "\n- rule: " + unsupportedRule + "\n- evidence: c-0002\n- why: x\n\n" +
			"### add: Tabs in configs\n- target: " + target + "\n- rule: " + supportedRule + "\n- evidence: c-0001\n- why: x\n",
		"Next: reformat the file with spaces\nVerdict: repeats",
		"Next: reformat the file with tabs\nVerdict: avoids",
	}}
	r, dir := replayReducer(t, base, target, fake)
	r.RequireOwnerEvidence = true
	// Room for either add alone, but one token short of both: only with the
	// held-back add wrongly counted does the supported one go over.
	r.Budget = fileCost(target) + tokenCost(unsupportedRule) + tokenCost(supportedRule) - 1
	path, _, err := r.Run(context.Background(), window, dir)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(path)
	if !strings.Contains(string(md), `"Gofmt" cites no observation the owner typed`) {
		t.Fatalf("the tool-only add should be unsupported:\n%s", md)
	}
	if strings.Contains(string(md), "## Over budget") {
		t.Errorf("an add that fits was flagged over budget:\n%s", md)
	}
	recs, err := LoadReplays(dir)
	if err != nil || len(recs) != 1 || recs[0].Title != "Tabs in configs" || recs[0].Held != "" {
		t.Errorf("replays = %+v, %v; want the supported add replayed and going ahead", recs, err)
	}
}

// A proposal an earlier review already holds back doesn't count against the
// budget either: apply.Plan skips it before the budget, so the reduce must
// too, or an add that fits is flagged over budget, denied its replay, and
// then applied by the plan with no replay record.
func TestBudgetCountsOnlyUnreviewedProposals(t *testing.T) {
	base := review.Resolve(t.TempDir())
	target := filepath.Join(base, "CLAUDE.md")
	writeFile(t, target, "- Existing rule.\n")
	pendingRule := strings.TrimSpace(strings.Repeat("Run gofmt before every commit. ", 8))
	supportedRule := "Use tabs, not spaces, in config files."
	fake := &model.Fake{Responses: []string{
		"## Proposals\n\n" +
			"### add: Gofmt\n- target: " + target + "\n- rule: " + pendingRule + "\n- evidence: c-0001\n- why: x\n\n" +
			"### add: Tabs in configs\n- target: " + target + "\n- rule: " + supportedRule + "\n- evidence: c-0001\n- why: x\n",
		"Next: reformat the file with spaces\nVerdict: repeats",
		"Next: reformat the file with tabs\nVerdict: avoids",
	}}
	r, dir := replayReducer(t, base, target, fake)
	r.Ledger.Reviews = []Review{{Kind: "add", Title: "Gofmt", Verdict: "pending",
		Via: "pr", Where: "https://example.com/pull/1"}}
	// Room for either add alone, but one token short of both: only with the
	// reviewed add wrongly counted does the second one go over.
	r.Budget = fileCost(target) + tokenCost(pendingRule) + tokenCost(supportedRule) - 1
	path, _, err := r.Run(context.Background(), replayWindow(), dir)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(path)
	if !strings.Contains(string(md), "## Already in review") || !strings.Contains(string(md), `- "Gofmt": already waiting for review`) {
		t.Errorf("the reviewed add should be dropped from the checks:\n%s", md)
	}
	if strings.Contains(string(md), "## Over budget") {
		t.Errorf("an add that fits was flagged over budget:\n%s", md)
	}
	recs, err := LoadReplays(dir)
	if err != nil || len(recs) != 1 || recs[0].Title != "Tabs in configs" || recs[0].Held != "" {
		t.Errorf("replays = %+v, %v; want only the unreviewed add replayed and going ahead", recs, err)
	}
}

// What "- displaces:" names is validated whenever it is set, even with no
// budget or a target with room, since the apply removes the displaced rule
// either way; and a reword that grows its rule counts the growth against
// the budget like an add.
func TestDisplacesIsCheckedWheneverSet(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "CLAUDE.md")
	other := filepath.Join(dir, "other.md")
	old := "Never push without asking."
	if err := os.WriteFile(target, []byte("- "+old+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := []Rule{
		{ID: "r-0001", Target: target, Text: old},
		{ID: "r-0002", Target: other, Text: "Elsewhere."},
	}
	add := func(displaces string) Proposal {
		return Proposal{Kind: "add", Title: "t", Rule: "Rebase onto upstream, never merge.", Displaces: displaces}
	}
	b := newBudgeter(0, rules) // no budget at all
	for _, tc := range []struct {
		p    Proposal
		want string
	}{
		{add("r-0999"), "isn't a rule on the ledger"},
		{add("r-0002"), "not in its target"},
		{add("r-0001"), ""},
		{add("r-0001"), "already displaces"},
	} {
		why := b.check(tc.p, target)
		if (tc.want == "") != (why == "") || !strings.Contains(why, tc.want) {
			t.Errorf("displaces %s: why = %q, want %q", tc.p.Displaces, why, tc.want)
		}
	}

	// A reword that grows its rule in a full target is held back like an
	// over-budget add; one that shrinks it always fits.
	grown := Proposal{Kind: "reword", Title: "t", Replaces: "r-0001", Rule: old + " " + strings.Repeat("More words. ", 20)}
	if why := overBudget(grown, target, fileCost(target)+10, rules); !strings.Contains(why, "has to name the rule it displaces") {
		t.Errorf("a growing reword passed a full target: %q", why)
	}
	shrunk := Proposal{Kind: "reword", Title: "t", Replaces: "r-0001", Rule: "Never push unasked."}
	if why := overBudget(shrunk, target, fileCost(target), rules); why != "" {
		t.Errorf("a shrinking reword was held back: %q", why)
	}
}

// An add that names a rule to displace trades one rule for another: the
// displaced text must leave the staged file or nothing is published, and the
// ledger records the displaced rule as retired with the add's review.
func TestDisplacedRuleMustLeaveTheStagedFile(t *testing.T) {
	ctx := context.Background()
	proposals := "## Proposals\n\n" +
		"### add: Swap in rebase\n- target: ~/dotfiles/.claude/CLAUDE.md\n" +
		"- rule: Rebase onto upstream, never merge.\n- displaces: r-0001\n- evidence: s1\n"
	write := func(f *applyFixture, content string) {
		f.editor.Apply = func(input string, dirs []string) (string, error) {
			var path string
			for _, line := range strings.Split(input, "\n") {
				if p, ok := strings.CutPrefix(line, "- edit: "); ok {
					path = p
				}
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return "", err
			}
			return "Applied[add]: Swap in rebase -- " + path, nil
		}
	}

	// The model added the rule but left the displaced one in place: the
	// apply fails and nothing is published.
	f := newApplyFixture(t, proposals)
	write(f, "# Rules\n\n- one\n- Rebase onto upstream, never merge.\n")
	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Routes[0].Skip != "" {
		t.Fatalf("held back: %q", plan.Routes[0].Skip)
	}
	if _, err := f.applier.Run(ctx, plan); err == nil || !strings.Contains(err.Error(), "still in") {
		t.Fatalf("err = %v, want the displaced rule named", err)
	}
	if len(f.pub.Changes) != 0 {
		t.Fatalf("published %+v with the displaced rule still present", f.pub.Changes)
	}

	// With the displaced rule really gone, the change publishes and the
	// displaced rule gets the same ledger history a retire gets.
	f2 := newApplyFixture(t, proposals)
	write(f2, "# Rules\n\n- Rebase onto upstream, never merge.\n")
	plan2, err := f2.applier.Plan(ctx, f2.run)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f2.applier.Run(ctx, plan2)
	if err != nil {
		t.Fatal(err)
	}
	l := &Ledger{Rules: f2.applier.Ledger.Rules}
	if n := l.RecordReviews(plan2, res, "pr"); n != 2 {
		t.Fatalf("recorded %d review(s), want the add and the displaced rule's retire: %+v", n, l.Reviews)
	}
	var retire Review
	for _, rv := range l.Reviews {
		if rv.Kind == "retire" {
			retire = rv
		}
	}
	if retire.Replaces != "r-0001" || retire.RuleHash != ruleHash("one") || retire.Where == "" ||
		retire.Verdict != "pending" || retire.Where != l.Reviews[0].Where {
		t.Errorf("the displaced rule's retire review = %+v", retire)
	}
}

// A rule records the models that answered in the sessions that earned it,
// and one earned only on models no recent session runs is listed for a
// trial without it.
func TestRulesRecordTheModelThatEarnedThem(t *testing.T) {
	fake := &model.Fake{Response: "NONE\n"}
	m := mapper(store.NewMem(), fake, nil)
	evs := []events.Event{
		{Kind: events.UserPrompt, Text: "go", Timestamp: "2026-11-01T00:00:00Z"},
		{Kind: events.AssistantText, Text: "a", Model: "model-old"},
		{Kind: events.AssistantText, Text: "b", Model: "model-old"},
		{Kind: events.AssistantText, Text: "c", Model: "<synthetic>"},
		{Kind: events.AssistantText, Text: "d", Model: "model-side", Sidechain: true},
	}
	res, err := m.MapSession(context.Background(), "claude", "s1", evs)
	if err != nil || strings.Join(res.Observations.Models, ",") != "model-old" {
		t.Fatalf("session models = %v, %v", res.Observations.Models, err)
	}

	// The models ride from the session to the run's evidence record, to
	// the review, and onto the rule when its bullet lands.
	so := session("s1", "2026-11-01", zshA)
	so.Models = []string{"model-old"}
	cl := &CandidateLedger{}
	evidence := runEvidence(cl.Cluster([]SessionObservations{so}, "2026-08-13", "2026-11-11"))
	if got := evidence.models([]string{"c-0001", "c-0404"}); strings.Join(got, ",") != "model-old" {
		t.Fatalf("evidence models = %v", got)
	}
	dir := t.TempDir()
	target := writeTarget(t, dir, "- Quote separators.")
	l := &Ledger{Reviews: []Review{{Kind: "add", Rule: "Quote separators.", RuleHash: ruleHash("Quote separators."),
		Evidence: []string{"s1"}, Models: []string{"model-old"}, Verdict: "merged"}}}
	if _, err := l.SyncTargets([]string{target}, time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	l.Rules = append(l.Rules, Rule{ID: "r-0002", Text: "No model on record."},
		Rule{ID: "r-0003", Text: "Still needed.", Models: []string{"model-old", "model-new"}})
	if strings.Join(l.Rules[0].Models, ",") != "model-old" {
		t.Fatalf("rule models = %v", l.Rules[0].Models)
	}
	r := &Reducer{Ledger: l, Clock: &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}}
	if in := r.BuildInput(nil); !strings.Contains(in, "; earned on model-old)") {
		t.Errorf("the reduce input doesn't say what a rule was earned on:\n%s", in)
	}
	if got := l.Outgrown([]string{"model-new"}); len(got) != 1 || got[0].ID != "r-0001" {
		t.Errorf("outgrown = %+v, want only the rule earned on a model nothing runs now", got)
	}
	if got := l.Outgrown([]string{"model-old"}); len(got) != 0 {
		t.Errorf("a rule earned on a model still in use was listed: %+v", got)
	}
	if got := l.Outgrown(nil); len(got) != 0 {
		t.Errorf("with no recent sessions to compare, nothing can be listed: %+v", got)
	}
}
