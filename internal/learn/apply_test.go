package learn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/review"
)

func TestParseProposals(t *testing.T) {
	md := "## Proposals\n\n" +
		"### add: Branch first\n- target: `~/.claude/CLAUDE.md`\n- rule: Branch before committing.\n- evidence: s1, s2\n\n" +
		"### skill: Timed practice\n- target: ~/.claude/skills/practice/SKILL.md\n- name: `/practice`\n- rule: steps\n\n" +
		"## Name collisions\n\n### not: a proposal\n- target: x\n"
	got := ParseProposals(md)
	if len(got) != 2 {
		t.Fatalf("got %d proposals, want 2: %+v", len(got), got)
	}
	if p := got[0]; p.Kind != "add" || p.Title != "Branch first" || p.Target != "~/.claude/CLAUDE.md" ||
		!strings.HasPrefix(p.Text, "### add: Branch first\n") || !strings.HasSuffix(p.Text, "- evidence: s1, s2") {
		t.Errorf("first = %+v", p)
	}
	if p := got[1]; p.Kind != "skill" || p.Name != "practice" {
		t.Errorf("second = %+v", p)
	}
	if got := ParseProposals("## Proposals\n\n(none)\n"); len(got) != 0 {
		t.Errorf("none parsed as %+v", got)
	}
}

func TestParseOutcomes(t *testing.T) {
	out := "Done.\n```\nApplied[add]: Branch -- first -- /wt/a.md, `/wt/b.md`\n" +
		"- Skipped[skill]: Timed practice -- it conflicts with -- another one\nApplied[retire]: No paths\n" +
		"Applied[graduate]: Script -- /wt/bin/x (not marked executable; no prose rule to replace)\n```\n"
	got := ParseOutcomes(out)
	if len(got) != 4 {
		t.Fatalf("got %+v", got)
	}
	if o := got[3]; strings.Join(o.Paths, ",") != "/wt/bin/x" || o.Detail != "not marked executable; no prose rule to replace" {
		t.Errorf("note after the paths = %+v", o)
	}
	if o := got[0]; !o.Applied || o.Title != "Branch -- first" || strings.Join(o.Paths, ",") != "/wt/a.md,/wt/b.md" {
		t.Errorf("applied = %+v", o)
	}
	if o := got[1]; o.Applied || o.Title != "Timed practice" || o.Detail != "it conflicts with -- another one" {
		t.Errorf("skipped = %+v", o)
	}
	if o := got[2]; !o.Applied || o.Title != "No paths" || o.Paths != nil {
		t.Errorf("no paths = %+v", o)
	}
}

func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "you")
	t.Setenv("GIT_COMMITTER_NAME", "you")
	t.Setenv("GIT_AUTHOR_EMAIL", "you@example.com")
	t.Setenv("GIT_COMMITTER_EMAIL", "you@example.com")
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// liveRepo makes a bare origin seeded with files and returns a clone of it.
func liveRepo(t *testing.T, base, name string, files map[string]string) string {
	t.Helper()
	origin := filepath.Join(base, name+".git")
	gitIn(t, base, "init", "-q", "--bare", "-b", "main", origin)
	live := filepath.Join(base, name)
	gitIn(t, base, "clone", "-q", origin, live)
	for rel, text := range files {
		writeFile(t, filepath.Join(live, rel), text)
	}
	gitIn(t, live, "add", "-A")
	gitIn(t, live, "commit", "-q", "-m", "seed")
	gitIn(t, live, "push", "-q", "origin", "HEAD:main")
	return review.Resolve(live)
}

// fakeApply stands in for the apply model: it writes each proposal's
// "- edit:" and "- script:" paths, and reports them.
func fakeApply(content string) func(input string, dirs []string) (string, error) {
	return func(input string, dirs []string) (string, error) {
		var out []string
		kind, title := "", ""
		for _, line := range strings.Split(input[strings.Index(input, "# PROPOSALS"):], "\n") {
			if rest, ok := strings.CutPrefix(line, "### "); ok {
				k, t, _ := strings.Cut(rest, ":")
				kind, title = k, strings.TrimSpace(t)
				continue
			}
			for _, prefix := range []string{"- edit: ", "- script: "} {
				if p, ok := strings.CutPrefix(line, prefix); ok {
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						return "", err
					}
					if err := os.WriteFile(p, []byte(content+" for "+title+"\n"), 0o644); err != nil {
						return "", err
					}
					out = append(out, "Applied["+kind+"]: "+title+" -- "+p)
				}
			}
		}
		return strings.Join(out, "\n"), nil
	}
}

type applyFixture struct {
	home, dot, kit, run string
	applier             *Applier
	editor              *model.FakeEditor
	pub                 *review.Fake
}

func newApplyFixture(t *testing.T, proposals string) *applyFixture {
	t.Helper()
	isolateGit(t)
	base := review.Resolve(t.TempDir())
	f := &applyFixture{home: base}
	f.dot = liveRepo(t, base, "dotfiles", map[string]string{".claude/CLAUDE.md": "# Rules\n\n- one\n"})
	f.kit = liveRepo(t, base, "kit", map[string]string{"skills/old/SKILL.md": "old\n", "bin/README.md": "scripts\n"})
	writeFile(t, filepath.Join(base, "loose", "notes.md"), "- loose rule\n") // in no repo
	f.run = filepath.Join(base, "state", "2026-11-11")
	writeFile(t, filepath.Join(f.run, "proposals.md"), proposals)
	f.editor = &model.FakeEditor{Apply: fakeApply("edited")}
	f.pub = &review.Fake{}
	f.applier = &Applier{
		Editor: f.editor, Model: "sonnet", Publisher: f.pub,
		Targets: []string{"~/dotfiles/.claude/CLAUDE.md", "~/loose/notes.md"},
		Skills:  filepath.Join(f.kit, "skills"), Bin: filepath.Join(f.kit, "bin"),
		Public: []string{f.kit}, Taken: []string{"Old"}, Home: base,
		Clock: &clock.Fake{T: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)},
	}
	return f
}

const mixedProposals = `## Proposals

### add: Branch first
- target: ~/dotfiles/.claude/CLAUDE.md
- rule: Branch before committing.
- evidence: s1, s2
- why: twice

### skill: Timed practice
- target: ~/kit/skills/practice/SKILL.md
- name: practice
- rule: steps
- evidence: s3

### graduate: Check the time math
- target: ~/kit/bin/check-time
- rule: a script sketch
- evidence: s4

### add: Somewhere else
- target: ~/other.md
- rule: x

### skill: Reuse a name
- name: old
- rule: x

### retire: Loose rule
- target: ~/loose/notes.md
- rule: loose rule
`

func TestApplyEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, mixedProposals)
	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Branch != "learn-2026-11-11" || plan.Ready() != 3 {
		t.Errorf("plan branch %s, %d ready", plan.Branch, plan.Ready())
	}
	if strings.Join(plan.Repos, ",") != f.dot+","+f.kit {
		t.Errorf("repos = %v", plan.Repos)
	}
	skips := map[string]string{}
	for _, r := range plan.Routes {
		skips[r.Title] = r.Skip
	}
	for title, want := range map[string]string{
		"Somewhere else": "isn't in learn.targets",
		"Reuse a name":   "already a skill",
		"Loose rule":     "isn't in a git checkout",
	} {
		if !strings.Contains(skips[title], want) {
			t.Errorf("%s skip = %q, want %q", title, skips[title], want)
		}
	}

	res, err := f.applier.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.editor.Calls) != 1 {
		t.Fatalf("editor called %d times", len(f.editor.Calls))
	}
	call := f.editor.Calls[0]
	if call.Model != "sonnet" || len(call.Dirs) != 2 {
		t.Errorf("edit call = model %s, dirs %v", call.Model, call.Dirs)
	}
	for _, want := range []string{"stands in for " + f.kit + " (public)", "stands in for " + f.dot + " (private)",
		"- edit: " + filepath.Join(call.Dirs[1], "skills", "practice", "SKILL.md"),
		"- script: " + filepath.Join(call.Dirs[1], "bin", "check-time")} {
		if !strings.Contains(call.Input, want) {
			t.Errorf("input misses %q", want)
		}
	}
	if strings.Contains(call.Input, "Somewhere else") {
		t.Error("a skipped proposal reached the model")
	}

	if len(f.pub.Changes) != 2 || len(res.Changes) != 2 {
		t.Fatalf("published %d, landed %+v", len(f.pub.Changes), res.Changes)
	}
	dotBody, kitBody := f.pub.Changes[0].Body, f.pub.Changes[1].Body
	if !strings.Contains(dotBody, "- applied add: Branch first (`.claude/CLAUDE.md`)") ||
		!strings.Contains(dotBody, "<details>") || !strings.Contains(dotBody, "- evidence: s1, s2") {
		t.Errorf("dotfiles body:\n%s", dotBody)
	}
	if strings.Contains(kitBody, "<details>") || strings.Contains(kitBody, "evidence") ||
		!strings.Contains(kitBody, "- applied skill: Timed practice (`skills/practice/SKILL.md`)") {
		t.Errorf("public kit body leaks proposals or misses the skill:\n%s", kitBody)
	}
	// The graduate rewrote the target too, so it's in both repos' lists.
	if !strings.Contains(kitBody, "graduate: Check the time math (`bin/check-time`)") {
		t.Errorf("kit body misses the script:\n%s", kitBody)
	}

	// Committed on the branch, never on the live checkout.
	if got := gitIn(t, f.dot, "show", plan.Branch+":.claude/CLAUDE.md"); !strings.Contains(got, "edited for Branch first") {
		t.Errorf("branch CLAUDE.md = %q", got)
	}
	if live, _ := os.ReadFile(filepath.Join(f.dot, ".claude", "CLAUDE.md")); string(live) != "# Rules\n\n- one\n" {
		t.Errorf("live file changed: %q", live)
	}
	if mode := gitIn(t, f.kit, "ls-tree", plan.Branch, "bin/check-time"); !strings.HasPrefix(mode, "100755") {
		t.Errorf("script not executable: %s", mode)
	}
	if mode := gitIn(t, f.kit, "ls-tree", plan.Branch, "bin/README.md"); !strings.HasPrefix(mode, "100644") {
		t.Errorf("an untouched file in bin changed mode: %s", mode)
	}
	if _, err := os.Stat(filepath.Join(f.run, "worktrees")); !os.IsNotExist(err) {
		t.Error("worktrees dir left behind")
	}

	var saved ApplyResult
	data, err := os.ReadFile(filepath.Join(f.run, "apply.json"))
	if err != nil || json.Unmarshal(data, &saved) != nil || len(saved.Changes) != 2 || len(saved.Outcomes) != 6 {
		t.Errorf("apply.json = %s, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(f.run, "apply.md")); err != nil {
		t.Error("apply.md missing")
	}
	if _, err := f.applier.Plan(ctx, f.run); !errors.Is(err, ErrApplied) {
		t.Errorf("second plan err = %v, want ErrApplied", err)
	}
}

func TestApplyNothingChangedDropsBranches(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, mixedProposals)
	f.editor.Apply = func(string, []string) (string, error) { return "Skipped[add]: Branch first -- conflicts", nil }
	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.applier.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.pub.Changes) != 0 || len(res.Changes) != 0 {
		t.Errorf("published %+v", f.pub.Changes)
	}
	if out := gitIn(t, f.dot, "branch", "--list", plan.Branch); out != "" {
		t.Errorf("branch kept with no change: %s", out)
	}
	unreported := 0
	for _, o := range res.Outcomes {
		if strings.Contains(o.Detail, "reported nothing") {
			unreported++
		}
	}
	if unreported != 2 {
		t.Errorf("outcomes = %+v, want the two unreported proposals named", res.Outcomes)
	}
}

func TestApplyCheckHoldsChange(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, mixedProposals)
	f.applier.Check = "! grep -q 'Timed practice'" // the diff on stdin
	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.applier.Run(ctx, plan)
	if err == nil || !strings.Contains(err.Error(), f.kit) {
		t.Fatalf("err = %v, want the held repo named", err)
	}
	if len(f.pub.Changes) != 1 || f.pub.Changes[0].Worktree.Repo != f.dot {
		t.Errorf("published %d changes, want only the dotfiles one", len(f.pub.Changes))
	}
	held := res.Changes[1]
	if !strings.Contains(held.Error, "learn.check failed") || held.Where != "" {
		t.Errorf("held change = %+v", held)
	}
	if _, err := os.Stat(plan.Worktrees[1]); err != nil {
		t.Errorf("held worktree removed: %v", err)
	}
}

func TestApplyModelFailureCleansUp(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, mixedProposals)
	f.editor.Apply = func(string, []string) (string, error) { return "", errors.New("rate limited") }
	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.applier.Run(ctx, plan); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v", err)
	}
	if out := gitIn(t, f.kit, "branch", "--list", plan.Branch); out != "" {
		t.Errorf("branch kept after a failed model run: %s", out)
	}
	if _, err := os.Stat(filepath.Join(f.run, "apply.json")); !os.IsNotExist(err) {
		t.Error("apply.json written for a failed run; a retry would be refused")
	}
}

func TestApplyAllSkippedCallsNoModel(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, "## Proposals\n\n### add: Elsewhere\n- target: ~/other.md\n- rule: x\n")
	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.applier.Run(ctx, plan)
	if err != nil || len(f.editor.Calls) != 0 || len(res.Outcomes) != 1 {
		t.Errorf("res %+v, err %v, %d model calls", res, err, len(f.editor.Calls))
	}
}

func TestDryRunInput(t *testing.T) {
	f := newApplyFixture(t, mixedProposals)
	plan, err := f.applier.Plan(context.Background(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	in := f.applier.BuildInput(plan, PlannedWorktrees(plan))
	want := "- ~/dotfiles/.claude/CLAUDE.md: edit at " + filepath.Join(plan.Worktrees[0], ".claude", "CLAUDE.md")
	if !strings.Contains(in, want) {
		t.Errorf("input misses %q:\n%s", want, in)
	}
	if _, err := os.Stat(plan.Worktrees[0]); !os.IsNotExist(err) {
		t.Error("planning created a worktree")
	}
}

func TestLatestRun(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "2026-11-04", "proposals.md"), "x")
	writeFile(t, filepath.Join(root, "2026-11-11", "input.md"), "reduce failed before proposals")
	writeFile(t, filepath.Join(root, "notes", "proposals.md"), "x")
	if got, err := LatestRun(root); err != nil || filepath.Base(got) != "2026-11-04" {
		t.Errorf("LatestRun = %q, %v", got, err)
	}
	if _, err := LatestRun(t.TempDir()); err == nil {
		t.Error("an empty root has a latest run")
	}
}

func TestParseProposalRule(t *testing.T) {
	got := ParseProposals("### skill: Practice\n- name: practice\n- rule: A procedure, in two parts:\n" +
		"  - **One:** do this.\n  - **Two:** then that.\n- evidence: s1\n")
	if len(got) != 1 || got[0].Rule != "A procedure, in two parts:\n- **One:** do this.\n- **Two:** then that." {
		t.Errorf("rule = %q", got[0].Rule)
	}
	if ruleHash("a  b\nc") != ruleHash("a b c") || ruleHash("") != "" {
		t.Error("ruleHash depends on wrapping, or hashes empty text")
	}
}

func TestApplyHoldsBackReviewed(t *testing.T) {
	f := newApplyFixture(t, mixedProposals)
	f.applier.Reviews = []Review{
		// Same title, still open: a second PR would duplicate it.
		{Kind: "add", Title: "Branch first", Verdict: "pending", Where: "https://example.com/pull/1"},
		// Retitled, same text, still open.
		{Kind: "skill", Title: "Old title", RuleHash: ruleHash("steps"), Verdict: "pending", Where: "https://example.com/pull/1"},
		// The same text was rejected; a different text under that title may return.
		{Kind: "graduate", Title: "Check the time math", RuleHash: ruleHash("a script sketch"),
			Verdict: "closed", Decided: "2026-11-05", Where: "https://example.com/pull/0"},
		{Kind: "add", Title: "Somewhere else", RuleHash: ruleHash("other text"), Verdict: "closed"},
	}
	plan, err := f.applier.Plan(context.Background(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	skips := map[string]string{}
	for _, r := range plan.Routes {
		skips[r.Title] = r.Skip
	}
	for title, want := range map[string]string{
		"Branch first":        "already waiting for review in https://example.com/pull/1",
		"Timed practice":      "already waiting for review",
		"Check the time math": "rejected in https://example.com/pull/0 on 2026-11-05",
		"Somewhere else":      "isn't in learn.targets", // its rejection was other text
	} {
		if !strings.Contains(skips[title], want) {
			t.Errorf("%s skip = %q, want %q", title, skips[title], want)
		}
	}
	if plan.Ready() != 0 || len(plan.Repos) != 0 {
		t.Errorf("ready %d, repos %v", plan.Ready(), plan.Repos)
	}
}

func TestRecordAndRefreshReviews(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, mixedProposals)
	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.applier.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	l := &Ledger{}
	if n := l.RecordReviews(plan, res, "pr"); n != 3 {
		t.Fatalf("recorded %d reviews, want 3: %+v", n, l.Reviews)
	}
	if n := l.RecordReviews(plan, res, "pr"); n != 0 {
		t.Errorf("recording the same run again added %d", n)
	}
	var add Review
	for _, rv := range l.Reviews {
		if rv.Title == "Branch first" {
			add = rv
		}
	}
	if add.Kind != "add" || add.Rule != "Branch before committing." || add.RuleHash == "" || add.Proposed != "2026-11-11" ||
		add.Where != "fake:learn-2026-11-11" || add.Commit == "" || add.Verdict != "pending" {
		t.Errorf("review = %+v", add)
	}

	// One answer per repo's change decides all three proposals.
	pub := &review.Fake{Verdicts: map[string]review.Verdict{"fake:learn-2026-11-11": review.Closed}}
	calls := 0
	lookup := func(via string) (review.Publisher, error) {
		calls++
		return pub, nil
	}
	today := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	n, err := l.RefreshReviews(ctx, lookup, today)
	if err != nil || n != 3 || calls != 2 {
		t.Errorf("refresh decided %d, err %v, %d lookups", n, err, calls)
	}
	for _, rv := range l.Reviews {
		if rv.Verdict != "closed" || rv.Decided != "2026-11-18" {
			t.Errorf("review after refresh = %+v", rv)
		}
	}

	// A publisher that can't answer leaves its reviews pending.
	l2 := &Ledger{Reviews: []Review{{Title: "x", Via: "pr", Where: "u", Verdict: "pending"}}}
	_, err = l2.RefreshReviews(ctx, func(string) (review.Publisher, error) {
		return &review.Fake{}, errors.New("gh: not signed in")
	}, today)
	if err == nil || l2.Reviews[0].Verdict != "pending" {
		t.Errorf("err %v, review %+v", err, l2.Reviews[0])
	}
}

// byRepo answers verdicts by repo, like branch review does.
type byRepo map[string]review.Verdict

func (b byRepo) Publish(context.Context, review.Change) (string, error) { return "", nil }
func (b byRepo) Verdict(_ context.Context, p review.Published) (review.Verdict, string, error) {
	return b[p.Repo], "", nil
}

// The same branch name in two repos is two reviews, asked about apart.
func TestRefreshKeysByRepo(t *testing.T) {
	l := &Ledger{Reviews: []Review{
		{Title: "a", Via: "branch", Where: "branch learn-x", Repo: "/r/rules", Verdict: "pending"},
		{Title: "b", Via: "branch", Where: "branch learn-x", Repo: "/r/kit", Verdict: "pending"},
	}}
	pub := byRepo{"/r/rules": review.Closed, "/r/kit": review.Merged}
	if _, err := l.RefreshReviews(context.Background(), func(string) (review.Publisher, error) { return pub, nil },
		time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if l.Reviews[0].Verdict != "closed" || l.Reviews[1].Verdict != "merged" {
		t.Errorf("reviews = %+v", l.Reviews)
	}
	if got := l.Reviews[1].Place(); got != "branch learn-x in /r/kit" {
		t.Errorf("place = %q", got)
	}
}
