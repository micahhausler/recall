package learn

import (
	"context"
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

func TestRepoNotePath(t *testing.T) {
	dir := "/home/you/src/notes/repo-notes"
	for remote, want := range map[string]string{
		"example.com/them/upstream": dir + "/example.com/them/upstream.md",
		"example.com/group/sub/x":   dir + "/example.com/group/sub/x.md",
		"":                          "",
		"~/src/project":             "",
		"/home/you/src/project":     "",
		"example.com/../etc":        "",
		"example.com/.hidden/x":     "",
		"example.com":               "",
		"host:22/them/x":            "",
		"localhost/them/x":          "",
		"docs/README":               "",
	} {
		got := RepoNotePath(dir, remote)
		if got != want {
			t.Errorf("RepoNotePath(%q) = %q, want %q", remote, got, want)
		}
		if want != "" && repoNoteRemote(dir, got) != remote {
			t.Errorf("repoNoteRemote(%q) = %q, want %q", got, repoNoteRemote(dir, got), remote)
		}
	}
	if RepoNotePath("", "example.com/them/upstream") != "" {
		t.Error("no repo notes directory should give no path")
	}
	if repoNoteRemote(dir, "/home/you/src/notes/CLAUDE.md") != "" || repoNoteRemote(dir, dir+"/example.com/them/x.txt") != "" {
		t.Error("a file outside the repo notes, or not Markdown, is no repo note")
	}
}

func TestDiscoverRepoNotes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo notes")
	if got, err := DiscoverRepoNotes(dir); err != nil || len(got) != 0 {
		t.Fatalf("a missing directory: %v, %v", got, err)
	}
	writeFile(t, filepath.Join(dir, "example.com", "them", "b.md"), "- b\n")
	writeFile(t, filepath.Join(dir, "example.com", "them", "a.md"), "- a\n")
	writeFile(t, filepath.Join(dir, "README.md"), "about\n")
	writeFile(t, filepath.Join(dir, "example.com", "them", "notes.txt"), "x\n")
	got, err := DiscoverRepoNotes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Remote != "example.com/them/a" || got[1].Remote != "example.com/them/b" {
		t.Fatalf("repo_notes = %+v", got)
	}
	if !got[0].RepoNote || got[0].Root != "" || got[0].Files[0].Path != filepath.Join(dir, "example.com", "them", "a.md") || got[0].Files[0].New {
		t.Errorf("repo note = %+v", got[0])
	}
}

func TestRepoNoteContext(t *testing.T) {
	home := review.Resolve(t.TempDir())
	upstream := mkdir(t, home, "src", "upstream")
	write(t, filepath.Join(mkdir(t, upstream, ".git"), "config"), "[remote \"origin\"]\n\turl = git@example.com:them/upstream.git\n")
	other := mkdir(t, home, "src", "other")
	write(t, filepath.Join(mkdir(t, other, ".git"), "config"), "[remote \"origin\"]\n\turl = git@example.com:them/other.git\n")
	dir := filepath.Join(home, "src", "notes", "repo-notes")
	writeFile(t, filepath.Join(dir, "example.com", "them", "upstream.md"), "- Run make test, not go test.\n")

	got, err := RepoNoteContext(dir, mkdir(t, upstream, "pkg"), home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Rules for example.com/them/upstream") || !strings.Contains(got, "- Run make test, not go test.") {
		t.Errorf("context = %q", got)
	}
	for name, cwd := range map[string]string{"another repo": other, "outside any repo": mkdir(t, home, "notes")} {
		if got, err := RepoNoteContext(dir, cwd, home); err != nil || got != "" {
			t.Errorf("%s: context = %q, %v", name, got, err)
		}
	}
	writeFile(t, filepath.Join(dir, "example.com", "them", "other.md"), "# other\n\n- Use tabs.\n")
	if got, _ := RepoNoteContext(dir, other, home); strings.Count(got, "# ") != 1 || !strings.Contains(got, "- Use tabs.") {
		t.Errorf("a note's own heading: context = %q", got)
	}
	if got, _ := RepoNoteContext("", upstream, home); got != "" {
		t.Errorf("no repo notes directory: context = %q", got)
	}
}

func TestRepoNoteScope(t *testing.T) {
	path := "/home/you/src/notes/repo-notes/example.com/them/upstream.md"
	note := Repo{Remote: "example.com/them/upstream", RepoNote: true, Files: []RepoFile{{Path: path}}}
	rules := []Rule{{ID: "r-0001", Target: path, Text: "x"}}
	e := RunEvidence{
		"c-in":   {{Session: "s1", Day: "2026-11-01", Repo: "example.com/them/upstream", Harness: "claude"}},
		"c-kiro": {{Session: "s2", Day: "2026-11-01", Repo: "example.com/them/upstream", Harness: "kiro"}},
		"c-out":  {{Session: "s3", Day: "2026-11-02", Repo: "example.com/you/project", Harness: "claude"}},
		"c-none": {{Session: "s4", Day: "2026-11-03", Repo: "example.com/them/upstream"}},
	}
	for _, c := range []struct {
		name string
		p    Proposal
		want string
	}{
		{"add on the repo's sessions", Proposal{Kind: "add", Candidates: []string{"c-in"}}, ""},
		{"add on a Claude and a Kiro session", Proposal{Kind: "add", Candidates: []string{"c-in", "c-kiro"}}, ""},
		{"add on another repo's session", Proposal{Kind: "add", Candidates: []string{"c-in", "c-out"}}, "which ran in example.com/you/project"},
		{"add on Kiro sessions alone", Proposal{Kind: "add", Candidates: []string{"c-kiro"}}, "which only claude loads"},
		{"add on Kiro and an unknown harness", Proposal{Kind: "add", Candidates: []string{"c-kiro", "c-none"}}, "which only claude loads"},
		{"reword on the repo's sessions", Proposal{Kind: "reword", Replaces: "r-0001", Candidates: []string{"c-in"}}, ""},
		{"graduate", Proposal{Kind: "graduate", Replaces: "r-0001", Candidates: []string{"c-in"}}, "recall changes nothing in that repo"},
	} {
		g := &Gates{Repos: []Repo{note}, Home: "/home/you", NoteHarnesses: []string{"claude"}, Ledger: &Ledger{Rules: rules}}
		why := g.scopeProblem(c.p, path, e)
		if (c.want == "") != (why == "") || !strings.Contains(why, c.want) {
			t.Errorf("%s: scopeProblem = %q, want %q", c.name, why, c.want)
		}
	}
	// The harness table decides who loads notes: with kiro listed, Kiro
	// sessions alone suffice; with none, no add goes to a note.
	kiro := &Gates{Repos: []Repo{note}, Home: "/home/you", NoteHarnesses: []string{"claude", "kiro"}, Ledger: &Ledger{Rules: rules}}
	if why := kiro.scopeProblem(Proposal{Kind: "add", Candidates: []string{"c-kiro"}}, path, e); why != "" {
		t.Errorf("kiro loads notes: %q", why)
	}
	none := &Gates{Repos: []Repo{note}, Home: "/home/you", Ledger: &Ledger{Rules: rules}}
	if why := none.scopeProblem(Proposal{Kind: "add", Candidates: []string{"c-in"}}, path, e); !strings.Contains(why, "no enabled harness loads repo notes") {
		t.Errorf("no harness loads notes: %q", why)
	}

	s := &Scoping{Repos: []Repo{note}, Home: "/home/you"}
	if !s.Applies(path, Scope{Remote: "example.com/them/upstream"}) {
		t.Error("a repo note's rules should apply to its repo's sessions")
	}
	for _, sc := range []Scope{{Remote: "example.com/you/project"}, {CWD: "~/notes"}, {}} {
		if s.Applies(path, sc) {
			t.Errorf("a repo note's rules apply to a session in %+v", sc)
		}
	}
	// A skill in no listed checkout isn't scoped by a repo note, which has
	// no checkout of its own.
	skill := Proposal{Kind: "skill", Target: "/home/you/.claude/skills/x/SKILL.md", Candidates: []string{"c-out"}}
	if why := scopeProblem(skill, skill.Target, []Repo{note}, e, "/home/you", rules); why != "" {
		t.Errorf("a global skill was scoped to a repo note: %q", why)
	}
}

// A lesson from a repo recall doesn't change lands in that repo's notes,
// staged in the owner repo that holds the repo notes, and the repo itself is
// left as it was.
func TestRepoNoteEndToEnd(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	base := review.Resolve(t.TempDir())
	notes := liveRepo(t, base, "notes", map[string]string{"CLAUDE.md": "# Notes\n", "repo-notes/README.md": "Repo notes.\n"})
	upstream := liveRepo(t, base, "upstream", map[string]string{"CLAUDE.md": "# Upstream\n", "main.go": "package main\n"})
	repo, err := DiscoverRepo(notes)
	if err != nil {
		t.Fatal(err)
	}
	noteDir := filepath.Join(notes, "repo notes")
	global := filepath.Join(notes, "CLAUDE.md")
	note := filepath.Join(noteDir, "example.com", "them", "upstream.md")

	here := session("s1", "2026-11-01", zshA)
	here.Scope = Scope{Repo: "~/upstream", Remote: "example.com/them/upstream"}
	here.Observations[0].Source = SourceOwner
	elsewhere := session("s2", "2026-11-02", "Backups must never delete data before the upload is confirmed.")
	elsewhere.Scope = Scope{Remote: "example.com/you/project"}
	elsewhere.Observations[0].Source = SourceOwner
	kiro := session("s3", "2026-11-03", "Timed practice needs the start time asked for up front.")
	kiro.Harness = "kiro"
	kiro.Scope = Scope{Remote: "example.com/them/other"}
	kiro.Observations[0].Source = SourceOwner

	fake := &model.Fake{Response: "## Proposals\n\n" +
		"### add: Quote separators upstream\n- target: " + note + "\n- rule: Quote separators.\n- evidence: c-0001\n- why: twice\n\n" +
		"### add: Upstream on outside evidence\n- target: " + note + "\n- rule: Confirm uploads.\n- evidence: c-0001, c-0002\n- why: once\n"}
	fakeClock := &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	gates := Gates{Targets: []string{global}, Repos: []Repo{repo}, Home: base, RepoNotes: noteDir, NoteHarnesses: []string{"claude"}, Ledger: &Ledger{}}
	r := &Reducer{Gates: gates, Runner: fake, Clock: fakeClock}
	dir := filepath.Join(base, "state", "2026-11-11")
	path, _, err := r.Run(ctx, []SessionObservations{here, elsewhere, kiro}, dir)
	if err != nil {
		t.Fatal(err)
	}
	in := fake.Calls[0].Input
	if !strings.Contains(in, "Repo notes, each the owner's file for one repo recall doesn't change") ||
		!strings.Contains(in, "- "+note+" (for example.com/them/upstream; doesn't exist yet)") {
		t.Errorf("reduce input should offer the upstream repo note:\n%s", in)
	}
	if strings.Contains(in, "them/other.md") {
		t.Error("reduce input offers a repo note for a repo only Kiro sessions ran in")
	}
	if !strings.Contains(in, "you/project.md (for example.com/you/project; doesn't exist yet)") {
		t.Error("reduce input should offer a repo note for each unlisted repo a Claude candidate rests on")
	}
	md, _ := os.ReadFile(path)
	if !strings.Contains(string(md), `"Upstream on outside evidence": it targets `+note+" but cites session s2, which ran in example.com/you/project") {
		t.Errorf("proposals.md should flag the repo note rule on another repo's evidence:\n%s", md)
	}

	before := snapshot(t, upstream)
	editor := &model.FakeEditor{Apply: fakeApply("- Quote separators.")}
	pub := &review.Fake{}
	a := &Applier{Gates: Gates{Targets: []string{global}, Repos: []Repo{repo}, Home: base, RepoNotes: noteDir, NoteHarnesses: []string{"claude"}, Ledger: &Ledger{}},
		Editor: editor, Model: "sonnet", Publisher: pub, Clock: fakeClock}
	plan, err := a.Plan(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range plan.Routes {
		switch route.Title {
		case "Quote separators upstream":
			if route.Skip != "" || len(route.Edit) != 1 || route.Edit[0] != note {
				t.Errorf("the repo note add routes to %v, skip %q", route.Edit, route.Skip)
			}
		case "Upstream on outside evidence":
			if !strings.Contains(route.Skip, "which ran in example.com/you/project") {
				t.Errorf("the misplaced repo note rule wasn't held back: %q", route.Skip)
			}
		}
	}
	if len(plan.Repos) != 1 || plan.Repos[0] != notes {
		t.Fatalf("plan touches %v, want only the owner repo", plan.Repos)
	}
	if _, err := a.Run(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if len(pub.Changes) != 1 {
		t.Fatalf("published %d changes, want one", len(pub.Changes))
	}
	ch := pub.Changes[0]
	if !strings.Contains(ch.Body, "Repo notes change here") || !strings.Contains(ch.Body, "- example.com/them/upstream") {
		t.Errorf("review body should name the repo note's repo:\n%s", ch.Body)
	}
	files := gitIn(t, notes, "--git-dir", filepath.Join(notes, ".git"), "show", "--name-only", "--format=", ch.Worktree.Branch)
	if !strings.Contains(files, "repo notes/example.com/them/upstream.md") {
		t.Errorf("the commit holds %q, want the new repo note", files)
	}
	if after := snapshot(t, upstream); after != before {
		t.Errorf("the repo the repo note is for changed:\nbefore %s\nafter  %s", before, after)
	}
}

// snapshot is a checkout's state as git and the filesystem see it.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", "git status --porcelain --ignored; git for-each-ref; git config --local --list; git notes list; find . -path ./.git -prune -o -type f -print | sort | xargs cat")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("snapshot: %v\n%s", err, out)
	}
	return string(out)
}

// The reduce and the apply add the same repo notes for a run's proposals,
// offered or not, and none for a target that isn't one.
func TestAddProposedRepoNotes(t *testing.T) {
	dir := "/home/you/notes/repo-notes"
	g := &Gates{RepoNotes: dir}
	g.addProposedRepoNotes([]Proposal{
		{Target: "~/notes/repo-notes/example.com/them/upstream.md"},
		{Target: "~/notes/repo-notes/docs/README.md"},
		{Target: "~/.agents/base.md"},
	}, func(p string) string { return normTarget(p, "/home/you") })
	if len(g.Repos) != 1 || g.Repos[0].Remote != "example.com/them/upstream" || !g.Repos[0].RepoNote {
		t.Errorf("repos = %+v", g.Repos)
	}
}

// A repo note under a public path is refused even when a symlink hides it.
func TestPublicNote(t *testing.T) {
	base := t.TempDir()
	real := mkdir(t, base, "src", "dotfiles")
	link := filepath.Join(base, "dotfiles")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(real, "repo-notes", "example.com", "them", "upstream.md")
	if why := publicNote(note, []string{link}, review.Resolve); !strings.Contains(why, "keep.public") {
		t.Errorf("public through a symlink: %q", why)
	}
	if why := publicNote(note, []string{filepath.Join(base, "src", "other")}, review.Resolve); why != "" {
		t.Errorf("not public: %q", why)
	}
}
