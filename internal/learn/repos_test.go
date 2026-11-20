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

func TestDiscoverRepo(t *testing.T) {
	root := review.Resolve(t.TempDir())
	write(t, filepath.Join(mkdir(t, root, ".git"), "config"), "[remote \"origin\"]\n\turl = https://example.com/you/project.git\n")
	write(t, filepath.Join(root, "AGENTS.md"), "- a rule\n")
	if err := os.Symlink("AGENTS.md", filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	rules := mkdir(t, root, ".claude", "rules")
	write(t, filepath.Join(rules, "go.md"), "---\npaths:\n  - \"internal/**/*.go\"\n  - cmd/**\n---\n- Table tests.\n")
	write(t, filepath.Join(rules, "docs.md"), "---\npaths: [\"docs/**\", '*.md']\n---\n- One sentence per line.\n")
	write(t, filepath.Join(rules, "always.md"), "- Applies everywhere.\n")
	write(t, filepath.Join(mkdir(t, root, "internal", "store"), "CLAUDE.md"), "- nested\n")
	write(t, filepath.Join(mkdir(t, root, "node_modules", "dep"), "CLAUDE.md"), "- someone else's\n")
	write(t, filepath.Join(mkdir(t, root, "a", "b", "c", "d"), "CLAUDE.md"), "- too deep\n")

	repo, err := DiscoverRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Remote != "example.com/you/project" {
		t.Errorf("remote = %q", repo.Remote)
	}
	var got []string
	for _, f := range repo.Files {
		rel, _ := filepath.Rel(root, f.Path)
		got = append(got, rel+" "+strings.Join(f.Globs, ","))
	}
	want := []string{
		"AGENTS.md ", // CLAUDE.md is a second name for it
		".claude/rules/always.md ",
		".claude/rules/docs.md docs/**,*.md",
		".claude/rules/go.md internal/**/*.go,cmd/**",
		"internal/store/CLAUDE.md ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("files:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(repo.Paths()) != len(want) {
		t.Errorf("paths = %v", repo.Paths())
	}

	// A repo with no context file offers a new one, which isn't a ledger
	// target until it exists.
	bare := review.Resolve(t.TempDir())
	mkdir(t, bare, ".git")
	empty, err := DiscoverRepo(bare)
	if err != nil || len(empty.Files) != 1 || !empty.Files[0].New || empty.Files[0].Path != filepath.Join(bare, "CLAUDE.md") || len(empty.Paths()) != 0 {
		t.Errorf("empty repo = %+v, %v", empty, err)
	}

	if !repo.holds("example.com/you/project", "/home/you") || repo.holds("example.com/you/other", "/home/you") || repo.holds("", "/home/you") {
		t.Error("a repo holds a session by its origin")
	}
	home := filepath.Dir(bare)
	if !empty.holds("~/"+filepath.Base(bare), home) || empty.holds("~/elsewhere", home) {
		t.Error("a repo without an origin holds a session by its path on this machine")
	}
}

// A repo's own rule is judged only against sessions that ran in that repo.
func TestRepoRulesJudgeOnlyTheirSessions(t *testing.T) {
	repo := Repo{Root: "/home/you/src/project", Remote: "example.com/you/project",
		Files: []RepoFile{{Path: "/home/you/src/project/CLAUDE.md"}}}
	global := Rule{ID: "r-0001", Target: "/home/you/.claude/CLAUDE.md", Text: "Ask first.", TextHash: textHash("Ask first.")}
	local := Rule{ID: "r-0002", Target: "/home/you/src/project/CLAUDE.md", Text: "Table tests.", TextHash: textHash("Table tests.")}
	fake := &model.Fake{Response: "NONE\n"}
	m := mapper(store.NewMem(), fake, []Rule{global, local})
	m.Home = "/home/you"
	m.Scoping = &Scoping{Repos: []Repo{repo}, Home: "/home/you"}
	m.Locate = func(dir string) (string, string) {
		if strings.HasPrefix(dir, "/home/you/src/project") {
			return "/home/you/src/project", "example.com/you/project"
		}
		return "", ""
	}
	ctx := context.Background()
	in := func(cwd string) []events.Event {
		return []events.Event{{Kind: events.UserPrompt, Text: "go", CWD: cwd, Timestamp: "2026-11-01T00:00:00Z"}}
	}
	if _, err := m.MapSession(ctx, "claude", "inside", in("/home/you/src/project/cmd")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MapSession(ctx, "claude", "outside", in("/home/you/notes")); err != nil {
		t.Fatal(err)
	}
	inside, outside := fake.Calls[0].Input, fake.Calls[1].Input
	if !strings.Contains(inside, "Rule[r-0001]") || !strings.Contains(inside, "Rule[r-0002]") {
		t.Error("a session in the repo should be judged against the global rule and the repo's own")
	}
	if !strings.Contains(outside, "Rule[r-0001]") || strings.Contains(outside, "Rule[r-0002]") {
		t.Error("a session outside the repo should not be judged against the repo's rule")
	}
}

// A new rule aimed at a repo's file must rest only on that repo's sessions:
// the reduce flags it and the apply holds it back.
func TestRepoTargetNeedsRepoEvidence(t *testing.T) {
	isolateGit(t)
	base := review.Resolve(t.TempDir())
	project := liveRepo(t, base, "project", map[string]string{"CLAUDE.md": "# Project\n\n- one\n"})
	dotfiles := liveRepo(t, base, "dotfiles", map[string]string{"CLAUDE.md": "- global\n"})
	repo, err := DiscoverRepo(project)
	if err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(dotfiles, "CLAUDE.md")
	local := filepath.Join(project, "CLAUDE.md")

	here := session("s1", "2026-11-01", zshA)
	here.Scope = Scope{Repo: "~/project"}
	here.Observations[0].Source = SourceOwner
	elsewhere := session("s2", "2026-11-02", "Backups must never delete data before the upload is confirmed.")
	elsewhere.Scope = Scope{Repo: "~/dotfiles"}
	elsewhere.Observations[0].Source = SourceOwner

	fake := &model.Fake{Response: "## Proposals\n\n" +
		"### add: Local and right\n- target: " + local + "\n- rule: Quote separators.\n- evidence: c-0001\n\n" +
		"### add: Local on outside evidence\n- target: " + local + "\n- rule: Confirm uploads.\n- evidence: c-0001, c-0002\n\n" +
		"### add: Global but local\n- target: " + global + "\n- rule: Quote separators here.\n- evidence: c-0001\n\n" +
		"### add: Global and right\n- target: " + global + "\n- rule: Confirm uploads everywhere.\n- evidence: c-0001, c-0002\n"}
	fakeClock := &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	r := &Reducer{Runner: fake, Ledger: &Ledger{}, Targets: []string{global}, Repos: []Repo{repo}, Home: base, Clock: fakeClock}
	dir := filepath.Join(base, "state", "2026-11-11")
	path, _, err := r.Run(context.Background(), []SessionObservations{here, elsewhere}, dir)
	if err != nil {
		t.Fatal(err)
	}
	in := fake.Calls[0].Input
	for _, want := range []string{
		"Global, loaded in every session:\n- " + global,
		"Repo ~/project, loaded only in sessions that run there:\n- " + local,
		"; repo ~/project)",
	} {
		if !strings.Contains(in, want) {
			t.Errorf("reduce input misses %q", want)
		}
	}
	md, _ := os.ReadFile(path)
	text := string(md)
	if !strings.Contains(text, "## Scope problems") || !strings.Contains(text, `"Local on outside evidence": it targets `+local+" but cites session s2, which ran in ~/dotfiles") {
		t.Errorf("proposals.md should flag the misplaced rule:\n%s", text)
	}
	if !strings.Contains(text, `"Global but local" targets a global file, and every session behind it ran in ~/project; it belongs in that repo's own context file, and the apply holds it back`) {
		t.Errorf("proposals.md should flag the global rule that is really local:\n%s", text)
	}
	if strings.Contains(text, "Off-target") || strings.Count(text, "it targets") != 1 || strings.Count(text, "targets a global file") != 1 {
		t.Errorf("only the two scope findings should be reported:\n%s", text)
	}

	a := &Applier{Targets: []string{global}, Repos: []Repo{repo}, Home: base, Clock: fakeClock}
	plan, err := a.Plan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range plan.Routes {
		switch route.Title {
		case "Local on outside evidence":
			if !strings.Contains(route.Skip, "which ran in ~/dotfiles") {
				t.Errorf("the misplaced rule wasn't held back: %q", route.Skip)
			}
		case "Global but local":
			if !strings.Contains(route.Skip, "it belongs in that repo's own context file") {
				t.Errorf("the global rule on one repo's evidence wasn't held back: %q", route.Skip)
			}
		default:
			if route.Skip != "" {
				t.Errorf("%q was held back: %s", route.Title, route.Skip)
			}
		}
		if route.Title == "Local and right" && (len(route.Edit) != 1 || route.Edit[0] != local) {
			t.Errorf("the repo's rule routes to %v, want its own context file", route.Edit)
		}
	}
	if len(plan.Repos) != 2 {
		t.Errorf("plan touches %v, want both checkouts", plan.Repos)
	}
}

// Placement checks cover every kind that lands somewhere repo-scoped: a
// reword of a repo rule, a changed skill inside a listed checkout, and a
// graduate of a repo rule must cite only that repo's sessions; a retire
// removes text and is never scope-checked.
func TestScopeProblemCoversRewordSkillGraduate(t *testing.T) {
	local := "/home/you/src/project/CLAUDE.md"
	repo := Repo{Root: "/home/you/src/project", Remote: "example.com/you/project",
		Files: []RepoFile{{Path: local}}}
	skill := "/home/you/src/project/.claude/skills/x/SKILL.md"
	rules := []Rule{
		{ID: "r-0001", Target: local, Text: "x"},
		{ID: "r-0002", Target: "/home/you/.claude/CLAUDE.md", Text: "y"},
	}
	e := RunEvidence{
		"c-in":  {{Session: "s1", Day: "2026-11-01", Repo: "example.com/you/project"}},
		"c-out": {{Session: "s2", Day: "2026-11-02", Repo: "~/elsewhere"}},
	}
	cases := []struct {
		name     string
		p        Proposal
		target   string
		heldBack bool
	}{
		{"reword on outside evidence", Proposal{Kind: "reword", Replaces: "r-0001", Candidates: []string{"c-out"}}, local, true},
		{"reword on repo evidence", Proposal{Kind: "reword", Replaces: "r-0001", Candidates: []string{"c-in"}}, local, false},
		{"skill in the repo on outside evidence", Proposal{Kind: "skill", Target: skill, Candidates: []string{"c-out"}}, skill, true},
		{"skill in the repo on repo evidence", Proposal{Kind: "skill", Target: skill, Candidates: []string{"c-in"}}, skill, false},
		{"skill outside any repo", Proposal{Kind: "skill", Target: "/home/you/.claude/skills/x/SKILL.md", Candidates: []string{"c-out"}},
			"/home/you/.claude/skills/x/SKILL.md", false},
		{"new skill with only a name", Proposal{Kind: "skill", Name: "x", Candidates: []string{"c-out"}}, "", false},
		{"graduate of a repo rule on outside evidence", Proposal{Kind: "graduate", Replaces: "r-0001", Candidates: []string{"c-out"}}, "", true},
		{"graduate of a repo rule on repo evidence", Proposal{Kind: "graduate", Replaces: "r-0001", Candidates: []string{"c-in"}}, "", false},
		{"graduate of a global rule", Proposal{Kind: "graduate", Replaces: "r-0002", Candidates: []string{"c-out"}}, "", false},
		{"retire is never scope-checked", Proposal{Kind: "retire", Replaces: "r-0001", Candidates: []string{"c-out"}}, local, false},
	}
	for _, c := range cases {
		why := scopeProblem(c.p, c.target, []Repo{repo}, e, "/home/you", rules)
		if (why != "") != c.heldBack {
			t.Errorf("%s: scopeProblem = %q, want held back %v", c.name, why, c.heldBack)
		}
	}
}
