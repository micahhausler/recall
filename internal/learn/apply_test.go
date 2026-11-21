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
	if strings.Join(got[0].Evidence, ",") != "s1,s2" {
		t.Errorf("evidence = %v", got[0].Evidence)
	}
	reword := ParseProposals("### reword: Ask first\n- target: x\n- replaces: Rule[r-0012]\n- rule: Ask.\n")
	if len(reword) != 1 || reword[0].Replaces != "r-0012" {
		t.Errorf("replaces = %+v", reword)
	}
	if got := ParseProposals("## Proposals\n\n(none)\n"); len(got) != 0 {
		t.Errorf("none parsed as %+v", got)
	}
}

// An evidence line ExpandEvidence rewrote carries labels ("tool-derived
// repeats") beside the session ids, and an ablation retire cites the rule id
// it removes; neither may leak into Evidence, which feeds Review.Evidence
// and then Rule.SourceSessions on the ledger.
func TestParseProposalsEvidenceCarriesOnlySessionIDs(t *testing.T) {
	expanded := ParseProposals("### add: Quote for zsh\n- target: x\n- rule: Quote.\n" +
		"- evidence: c-0001 (tool-derived repeats; sessions s-a, s-b), c-0002 (sessions s-c)\n")
	if len(expanded) != 1 {
		t.Fatalf("got %d proposals, want 1", len(expanded))
	}
	if got := strings.Join(expanded[0].Candidates, ","); got != "c-0001,c-0002" {
		t.Errorf("candidates = %q, want c-0001,c-0002", got)
	}
	if got := strings.Join(expanded[0].Evidence, ","); got != "s-a,s-b,s-c" {
		t.Errorf("evidence = %q, want only the session ids s-a,s-b,s-c", got)
	}

	retire := ParseProposals("### retire: Ablation\n- target: x\n- replaces: r-0001\n- rule: Old.\n" +
		"- evidence: r-0001, s-x, s-y\n")
	if len(retire) != 1 {
		t.Fatalf("got %d proposals, want 1", len(retire))
	}
	if got := strings.Join(retire[0].Evidence, ","); got != "s-x,s-y" {
		t.Errorf("evidence = %q, want only the session ids s-x,s-y", got)
	}
	if retire[0].Candidates != nil {
		t.Errorf("candidates = %v, want none", retire[0].Candidates)
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
		Ledger: &Ledger{Rules: []Rule{
			{ID: "r-0001", Target: filepath.Join(f.dot, ".claude", "CLAUDE.md"), Text: "one"},
			{ID: "r-0002", Target: filepath.Join(base, "loose", "notes.md"), Text: "loose rule"},
			{ID: "r-0007", Target: filepath.Join(f.dot, ".claude", "CLAUDE.md"), Text: "Ask before a push."},
			{ID: "r-0008", Target: filepath.Join(f.dot, ".claude", "CLAUDE.md"), Text: "Never force-push."},
		}},
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
- replaces: r-0001
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
- replaces: r-0002
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
	// The two worktrees, and the shadow folder the dotfiles' .claude
	// directory is copied to, since the harness won't edit under one.
	if call.Model != "sonnet" || len(call.Dirs) != 3 || filepath.Base(call.Dirs[2]) != shadowDir {
		t.Errorf("edit call = model %s, dirs %v", call.Model, call.Dirs)
	}
	for _, l := range strings.Split(call.Input, "\n") {
		if strings.HasPrefix(l, "- edit: ") && strings.Contains(l, "/.claude/") {
			t.Errorf("input names a path under .claude, which the harness won't edit: %s", l)
		}
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

// A repo that ignores everything must still get every routed file into the
// commit: the apply force-adds each one and names it in the pull request
// body, so the owner can fix the ignore file.
func TestApplyCommitsIgnoredFiles(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, ignoredRepoProposals)
	writeFile(t, filepath.Join(f.kit, ".gitignore"), "*\n")
	gitIn(t, f.kit, "add", "-f", ".gitignore")
	gitIn(t, f.kit, "commit", "-q", "-m", "ignore everything")
	gitIn(t, f.kit, "push", "-q", "origin", "HEAD:main")

	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.applier.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range res.Changes {
		if l.Error != "" {
			t.Fatalf("change failed: %+v", l)
		}
	}
	for _, rel := range []string{"bin/check-time", "skills/practice/SKILL.md"} {
		if got := gitIn(t, f.kit, "ls-tree", plan.Branch, rel); got == "" {
			t.Errorf("%s missing from the commit", rel)
		}
	}
	var kitBody string
	for _, c := range f.pub.Changes {
		if c.Worktree != nil && c.Worktree.Repo == f.kit {
			kitBody = c.Body
		}
	}
	if !strings.Contains(kitBody, "An ignore rule matches these files") ||
		!strings.Contains(kitBody, "- `bin/check-time`") ||
		!strings.Contains(kitBody, "- `skills/practice/SKILL.md`") {
		t.Errorf("kit body doesn't name the ignored files:\n%s", kitBody)
	}
}

const ignoredRepoProposals = `## Proposals

### skill: Timed practice
- target: ~/kit/skills/practice/SKILL.md
- name: practice
- rule: steps
- evidence: s3

### graduate: Check the time math
- target: ~/kit/bin/check-time
- replaces: r-0001
- rule: a script sketch
- evidence: s4
`

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
	f.applier.Ledger.Reviews = []Review{
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

	// Each proposal has its own commit, so each is asked about on its own,
	// through one publisher.
	pub := &review.Fake{Verdicts: map[string]review.Verdict{"fake:learn-2026-11-11": review.Closed}}
	calls := 0
	lookup := func(via string) (review.Publisher, error) {
		calls++
		return pub, nil
	}
	today := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	n, err := l.RefreshReviews(ctx, lookup, today)
	if err != nil || n != 3 || calls != 1 || pub.Asked != 3 {
		t.Errorf("refresh decided %d, err %v, %d lookups, %d verdicts asked", n, err, calls, pub.Asked)
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

// An apply whose model echoes a reworded title still links the review back
// to its proposal: reviews are keyed on the plan's route, carried through
// the apply, not on an exact title echo, and they record the hash of the
// rule text that actually landed in the staged diff.
func TestReviewLinksByRouteNotTitleEcho(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, "## Proposals\n\n"+
		"### reword: Ask before pushing\n"+
		"- target: ~/dotfiles/.claude/CLAUDE.md\n"+
		"- replaces: r-0007\n"+
		"- rule: Ask before any push.\n"+
		"- evidence: s1\n")
	landed := "Ask before any push or force push."
	f.editor.Apply = func(input string, dirs []string) (string, error) {
		var path string
		for _, line := range strings.Split(input, "\n") {
			if p, ok := strings.CutPrefix(line, "- edit: "); ok {
				path = p
			}
		}
		if err := os.WriteFile(path, []byte("# Rules\n\n- one\n- "+landed+"\n"), 0o644); err != nil {
			return "", err
		}
		return "Applied[reword]: Ask before pushing, now tightened -- " + path, nil
	}
	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.applier.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 || strings.Join(res.Changes[0].Proposals, ",") != "Ask before pushing" {
		t.Fatalf("changes = %+v, want the proposal under its own title", res.Changes)
	}
	l := &Ledger{Rules: f.applier.Ledger.Rules}
	if n := l.RecordReviews(plan, res, "pr"); n != 1 {
		t.Fatalf("recorded %d reviews: %+v", n, l.Reviews)
	}
	rv := l.Reviews[0]
	if rv.Kind != "reword" || rv.Replaces != "r-0007" || rv.Title != "Ask before pushing" {
		t.Errorf("review lost its lineage behind a reworded title: %+v", rv)
	}
	if rv.RuleHash != ruleHash(landed) {
		t.Errorf("rule hash is of %q, want the text that landed (%q)", rv.Rule, landed)
	}
	// Open finds the pending review under the proposal's own title, so the
	// candidate it cited is held back while the owner decides.
	clusters := []Cluster{{ID: "c-0001", Proposed: []Cited{{Date: plan.Date, Kind: "reword", Title: "Ask before pushing"}}}}
	open, used := Open(clusters, l.Reviews)
	if used != 1 || len(open) != 0 {
		t.Errorf("open = %+v, used = %d; the pending review wasn't found", open, used)
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

// End-to-end on fakes and a local repo: the apply stages one commit per
// proposal, the owner drops one commit and merges the rest, and the ledger
// finds one proposal merged and one rejected.
func TestPerProposalVerdictsOnABranch(t *testing.T) {
	ctx := context.Background()
	isolateGit(t)
	base := review.Resolve(t.TempDir())
	repo := liveRepo(t, base, "dotfiles", map[string]string{
		".claude/CLAUDE.md": "# Rules\n\n- one\n",
		".claude/work.md":   "# Work\n\n- two\n",
	})
	rules := filepath.Join(repo, ".claude", "CLAUDE.md")
	work := filepath.Join(repo, ".claude", "work.md")
	run := filepath.Join(base, "state", "2026-11-11")
	writeFile(t, filepath.Join(run, "proposals.md"), "## Proposals\n\n"+
		"### add: Branch first\n- target: "+rules+"\n- rule: Branch before committing.\n- evidence: s1\n\n"+
		"### add: Vet first\n- target: "+work+"\n- rule: Run go vet before saying done.\n- evidence: s2\n")
	bullets := map[string]string{
		"Branch first": "Branch before committing.",
		"Vet first":    "Run go vet before saying done.",
	}
	editor := &model.FakeEditor{Apply: func(input string, dirs []string) (string, error) {
		var out []string
		title := ""
		for _, line := range strings.Split(input, "\n") {
			if rest, ok := strings.CutPrefix(line, "### add: "); ok {
				title = strings.TrimSpace(rest)
			}
			if p, ok := strings.CutPrefix(line, "- edit: "); ok {
				data, err := os.ReadFile(p)
				if err != nil {
					return "", err
				}
				if err := os.WriteFile(p, append(data, []byte("- "+bullets[title]+"\n")...), 0o644); err != nil {
					return "", err
				}
				out = append(out, "Applied[add]: "+title+" -- "+p)
			}
		}
		return strings.Join(out, "\n"), nil
	}}
	a := &Applier{
		Editor: editor, Model: "sonnet", Publisher: review.Branch{},
		Targets: []string{rules, work}, Home: base,
		Skills: filepath.Join(repo, "skills"), Bin: filepath.Join(repo, "bin"),
		Clock: &clock.Fake{T: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)},
	}
	plan, err := a.Plan(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 || len(res.Changes[0].Proposals) != 2 {
		t.Fatalf("changes = %+v", res.Changes)
	}
	ch := res.Changes[0]
	if got := gitIn(t, repo, "rev-list", "--count", plan.Branch, "^origin/main"); got != "2" {
		t.Fatalf("the branch carries %s commit(s), want one per proposal", got)
	}
	branchFirst, vetFirst := ch.Commits[plan.Routes[0].ID], ch.Commits[plan.Routes[1].ID]
	if branchFirst == "" || vetFirst == "" || branchFirst == vetFirst {
		t.Fatalf("commits = %+v, want one per proposal", ch.Commits)
	}
	if msg := gitIn(t, repo, "log", "-1", "--format=%s", branchFirst); msg != "add: Branch first" {
		t.Errorf("commit message = %q", msg)
	}

	l := &Ledger{}
	if n := l.RecordReviews(plan, res, "branch"); n != 2 {
		t.Fatalf("recorded %d reviews: %+v", n, l.Reviews)
	}
	for _, rv := range l.Reviews {
		if rv.Commit != ch.Commits["p-01"] && rv.Commit != ch.Commits["p-02"] {
			t.Errorf("review %q carries commit %q, want its own proposal's", rv.Title, rv.Commit)
		}
	}

	// The owner drops the second proposal's commit, merges the first, and
	// deletes the branch.
	gitIn(t, repo, "merge", "-q", branchFirst)
	gitIn(t, repo, "branch", "-q", "-D", plan.Branch)
	lookup := func(string) (review.Publisher, error) { return review.Branch{}, nil }
	n, err := l.RefreshReviews(ctx, lookup, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))
	if err != nil || n != 2 {
		t.Fatalf("refresh decided %d, %v", n, err)
	}
	got := map[string]review.Verdict{}
	for _, rv := range l.Reviews {
		got[rv.Title] = rv.Verdict
	}
	if got["Branch first"] != "merged" || got["Vet first"] != "closed" {
		t.Errorf("verdicts = %v, want the merged proposal merged and the dropped one rejected", got)
	}
	if live, _ := os.ReadFile(rules); !strings.Contains(string(live), "Branch before committing.") {
		t.Errorf("the merged rule never reached the live file: %q", live)
	}
}

// A merged change proves only that the change landed. A proposal whose rule
// text is absent from the merged base was taken out before the merge, and
// is recorded as rejected.
func TestMergedWithoutProposalTextIsRejected(t *testing.T) {
	ctx := context.Background()
	isolateGit(t)
	base := review.Resolve(t.TempDir())
	repo := liveRepo(t, base, "dotfiles", map[string]string{"CLAUDE.md": "# Rules\n\n- Kept rule.\n"})
	head := gitIn(t, repo, "rev-parse", "HEAD")
	// The dropped proposal's commit stays on the review branch, never
	// merged: the apply stages one commit per proposal, and the owner
	// dropped this one before merging.
	gitIn(t, repo, "checkout", "-q", "-b", "learn-x")
	writeFile(t, filepath.Join(repo, "notes.md"), "- Dropped rule.\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "add: Taken out")
	dropped := gitIn(t, repo, "rev-parse", "HEAD")
	gitIn(t, repo, "checkout", "-q", "main")
	l := &Ledger{Reviews: []Review{
		{Kind: "add", Title: "Kept", Rule: "Kept rule.", Via: "branch", Where: "branch learn-x",
			Repo: repo, Base: "origin/main", Commit: head, Verdict: "pending"},
		{Kind: "add", Title: "Taken out", Rule: "Dropped rule.", Via: "branch", Where: "branch learn-x",
			Repo: repo, Base: "origin/main", Commit: dropped, Verdict: "pending"},
	}}
	pub := &review.Fake{Verdicts: map[string]review.Verdict{"branch learn-x": review.Merged}}
	n, err := l.RefreshReviews(ctx, func(string) (review.Publisher, error) { return pub, nil },
		time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))
	if err != nil || n != 2 {
		t.Fatalf("refresh decided %d, %v", n, err)
	}
	if l.Reviews[0].Verdict != "merged" || l.Reviews[1].Verdict != "closed" {
		t.Errorf("verdicts = %q, %q; want the landed text merged and the missing text rejected",
			l.Reviews[0].Verdict, l.Reviews[1].Verdict)
	}
}

// Phase 4 gate: one ledger carries rules from both CLAUDE.md and a Kiro
// steering file, and one apply stages an edit to each.
func TestOneLedgerWritesClaudeAndKiroSteering(t *testing.T) {
	ctx := context.Background()
	isolateGit(t)
	base := review.Resolve(t.TempDir())
	home := liveRepo(t, base, "home", map[string]string{
		".claude/CLAUDE.md":        "# Rules\n\n- one\n",
		".kiro/steering/AGENTS.md": "---\ninclusion: always\n---\n\n# Steering\n\n- kiro rule\n",
	})
	claudeMD := filepath.Join(home, ".claude", "CLAUDE.md")
	steering := filepath.Join(home, ".kiro", "steering", "AGENTS.md")
	fakeClock := &clock.Fake{T: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}

	// Both targets' bullets land on the same ledger; the steering file's
	// frontmatter stays YAML, not a rule.
	l := &Ledger{}
	if _, err := l.SyncTargets([]string{claudeMD, steering}, fakeClock.Now()); err != nil {
		t.Fatal(err)
	}
	byText := map[string]string{}
	for _, r := range l.Rules {
		byText[r.Text] = r.Target
	}
	if len(l.Rules) != 2 || byText["one"] != claudeMD || byText["kiro rule"] != steering {
		t.Fatalf("rules = %+v", l.Rules)
	}

	run := filepath.Join(base, "state", "2026-11-11")
	writeFile(t, filepath.Join(run, "proposals.md"), "## Proposals\n\n"+
		"### add: Branch first\n- target: "+claudeMD+"\n- rule: Branch before committing.\n- evidence: s1\n\n"+
		"### add: Name the workspace\n- target: "+steering+"\n- rule: Name the workspace in the first prompt.\n- evidence: s2\n")
	editor := &model.FakeEditor{Apply: fakeApply("edited")}
	a := &Applier{
		Editor: editor, Model: "sonnet", Publisher: review.Branch{},
		Targets: []string{claudeMD, steering},
		Skills:  filepath.Join(home, "skills"), Bin: filepath.Join(home, "bin"),
		Home: base, Clock: fakeClock,
	}
	plan, err := a.Plan(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range plan.Routes {
		if r.Skip != "" {
			t.Fatalf("%q held back: %s", r.Title, r.Skip)
		}
	}
	res, err := a.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 || len(res.Changes[0].Proposals) != 2 {
		t.Fatalf("landed = %+v", res.Changes)
	}
	for _, rel := range []string{".claude/CLAUDE.md", ".kiro/steering/AGENTS.md"} {
		if got := gitIn(t, home, "show", plan.Branch+":"+rel); !strings.Contains(got, "edited for ") {
			t.Errorf("%s on the branch = %q, want the staged edit", rel, got)
		}
	}

	// The one ledger records both reviews, and merging the branch lands
	// both edits.
	l.RecordReviews(plan, res, "branch")
	if len(l.Reviews) != 2 {
		t.Fatalf("reviews = %+v", l.Reviews)
	}
	gitIn(t, home, "merge", "-q", "--no-edit", plan.Branch)
	if live, _ := os.ReadFile(claudeMD); !strings.Contains(string(live), "edited for Branch first") {
		t.Errorf("CLAUDE.md after the merge = %q", live)
	}
	if live, _ := os.ReadFile(steering); !strings.Contains(string(live), "edited for Name the workspace") {
		t.Errorf("steering file after the merge = %q", live)
	}
}

// A displaced bullet inside a steering file's YAML frontmatter is judged the
// same way by SyncTargets and hasBullet: configuration, not a rule.
func TestSyncAndHasBulletAgreeOnFrontmatter(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	data := []byte(strings.Join([]string{
		"---",
		"inclusion: fileMatch",
		"fileMatchPattern:",
		"- 'src/**'",
		"---",
		"",
		"- Run the build before saying done.",
	}, "\n") + "\n")
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
	l := &Ledger{}
	if _, err := l.SyncTargets([]string{target}, time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	synced := map[string]bool{}
	for _, r := range l.Rules {
		synced[r.Text] = true
	}
	for text, want := range map[string]bool{"'src/**'": false, "Run the build before saying done.": true} {
		if synced[text] != want {
			t.Errorf("sync found %q = %t, want %t", text, synced[text], want)
		}
		if got := hasBullet(data, text); got != want {
			t.Errorf("hasBullet(%q) = %t, want %t, the answer sync gives", text, got, want)
		}
	}
}

// A bullet added inside a code fence is an example, not a rule: landedRules
// reads the diff's new side by the same fence rules sync reads a target.
func TestLandedRulesIgnoresFencedBullets(t *testing.T) {
	plan := &Plan{Routes: []Route{
		{ID: "r1", Proposal: Proposal{Kind: "add", Title: "Vet first", Rule: "Run go vet before saying done."}},
		{ID: "r2", Proposal: Proposal{Kind: "add", Title: "Fenced", Rule: "Only in the fence, never landed."}},
	}}
	applied := []Outcome{{Route: "r1", Applied: true}, {Route: "r2", Applied: true}}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), strings.Join([]string{
		"# Rules",
		"",
		"- Run go vet before saying done.",
		"",
		"```",
		"- Only in the fence, never landed.",
		"```",
		"",
	}, "\n"))
	diff := strings.Join([]string{
		"diff --git a/CLAUDE.md b/CLAUDE.md",
		"--- a/CLAUDE.md",
		"+++ b/CLAUDE.md",
		"@@ -1,2 +1,8 @@",
		" # Rules",
		" ",
		"+- Run go vet before saying done.",
		"+",
		"+```",
		"+- Only in the fence, never landed.",
		"+```",
	}, "\n")
	landed := landedRules(plan, applied, diff, dir)
	if landed["r1"] != "Run go vet before saying done." {
		t.Errorf("r1 landed %q, want the unfenced bullet", landed["r1"])
	}
	if text, ok := landed["r2"]; ok {
		t.Errorf("the fenced bullet landed as %q; a fenced bullet is not a rule", text)
	}
}

// A hunk that starts inside an existing code fence shows only the closing
// fence line as context. The staged file, not the hunk, is the ground truth
// for fence state: a genuine bullet added after the fenced example must land,
// so lineage records the text that is in the file.
func TestLandedRulesSeesBulletAfterFencedExample(t *testing.T) {
	plan := &Plan{Routes: []Route{
		{ID: "r1", Proposal: Proposal{Kind: "add", Title: "Vet first", Rule: "Run go vet before saying done."}},
	}}
	applied := []Outcome{{Route: "r1", Applied: true}}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), strings.Join([]string{
		"# Rules",
		"",
		"An example:",
		"",
		"```",
		"go vet ./...",
		"```",
		"",
		"- Run go vet before saying done.",
		"",
	}, "\n"))
	diff := strings.Join([]string{
		"diff --git a/CLAUDE.md b/CLAUDE.md",
		"--- a/CLAUDE.md",
		"+++ b/CLAUDE.md",
		"@@ -6,3 +6,4 @@",
		" go vet ./...",
		" ```",
		" ",
		"+- Run go vet before saying done.",
	}, "\n")
	landed := landedRules(plan, applied, diff, dir)
	if landed["r1"] != "Run go vet before saying done." {
		t.Errorf("r1 landed %q, want the bullet added after the fenced example", landed["r1"])
	}
}

// A hunk inside a steering file's YAML frontmatter never shows the opening
// --- line, and a scanner restarted at the hunk can't know it is inside
// frontmatter. The staged file says so: a YAML list item added there is
// configuration, never a landed rule.
func TestLandedRulesSkipsFrontmatterHunk(t *testing.T) {
	plan := &Plan{Routes: []Route{
		{ID: "r1", Proposal: Proposal{Kind: "add", Title: "Docs too", Rule: "docs and examples included"}},
	}}
	applied := []Outcome{{Route: "r1", Applied: true}}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), strings.Join([]string{
		"---",
		"inclusion: fileMatch",
		"fileMatchPattern:",
		"- 'src/**'",
		"- docs and examples included",
		"---",
		"",
		"- Run the build before saying done.",
		"",
	}, "\n"))
	diff := strings.Join([]string{
		"diff --git a/AGENTS.md b/AGENTS.md",
		"--- a/AGENTS.md",
		"+++ b/AGENTS.md",
		"@@ -2,3 +2,4 @@",
		" inclusion: fileMatch",
		" fileMatchPattern:",
		" - 'src/**'",
		"+- docs and examples included",
	}, "\n")
	landed := landedRules(plan, applied, diff, dir)
	if text, ok := landed["r1"]; ok {
		t.Errorf("the frontmatter YAML item landed as %q; frontmatter is configuration, not a rule", text)
	}
}
