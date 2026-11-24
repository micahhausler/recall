package learn

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/review"
	"github.com/micahhausler/recall/internal/store"
)

func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	dir := filepath.Join(parts...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLocateCheckout(t *testing.T) {
	home := t.TempDir()
	repo := mkdir(t, home, "src", "project")
	write(t, filepath.Join(mkdir(t, repo, ".git"), "config"),
		"[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = https://example.com/them/project.git\n"+
			"[remote \"origin\"]\n\turl = git@example.com:you/project.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n")
	sub := mkdir(t, repo, "internal", "deep")

	// A worktree's .git is a file pointing into the main checkout, whose
	// commondir file names the main git directory.
	wt := mkdir(t, repo, ".worktree", "feature")
	mkdir(t, repo, ".git", "worktrees", "feature")
	write(t, filepath.Join(repo, ".git", "worktrees", "feature", "commondir"), "../..\n")
	write(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(repo, ".git", "worktrees", "feature")+"\n")

	locate := LocateCheckout(home)
	for name, dir := range map[string]string{
		"the root":           repo,
		"a subdirectory":     sub,
		"a worktree":         wt,
		"a removed worktree": filepath.Join(repo, ".worktree", "gone", "pkg"),
	} {
		root, remote := locate(dir)
		if root != repo || remote != "example.com/you/project" {
			t.Errorf("%s: root=%q remote=%q, want the main checkout and its origin", name, root, remote)
		}
	}
	if root, remote := locate(mkdir(t, home, "notes")); root != "" || remote != "" {
		t.Errorf("outside a checkout: root=%q remote=%q", root, remote)
	}
}

// A dotfiles checkout rooted at the home directory claims no session: the
// walk up from a session's directory stops at home.
func TestLocateCheckoutStopsAtHome(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(mkdir(t, home, ".git"), "config"),
		"[remote \"origin\"]\n\turl = https://example.com/you/dotfiles.git\n")
	locate := LocateCheckout(home)
	if root, remote := locate(mkdir(t, home, "notes", "x")); root != "" || remote != "" {
		t.Errorf("~/.git claimed a session under home: root=%q remote=%q", root, remote)
	}
	if root, remote := locate(home); root != "" || remote != "" {
		t.Errorf("~/.git claimed a session at home: root=%q remote=%q", root, remote)
	}
	// A checkout below home is still found.
	repo := mkdir(t, home, "src", "p")
	write(t, filepath.Join(mkdir(t, repo, ".git"), "config"),
		"[remote \"origin\"]\n\turl = https://example.com/you/p.git\n")
	if root, remote := locate(mkdir(t, repo, "sub")); root != repo || remote != "example.com/you/p" {
		t.Errorf("below home: root=%q remote=%q", root, remote)
	}
}

// A worktree's main checkout comes from its git directory's commondir file,
// not from where the word "worktrees" happens to sit in the path.
func TestLocateCheckoutWorktreeByCommonDir(t *testing.T) {
	root := mkdir(t, t.TempDir(), "ws", "worktrees", "x", "src", "pkg")
	write(t, filepath.Join(mkdir(t, root, ".git"), "config"),
		"[remote \"origin\"]\n\turl = https://example.com/you/pkg.git\n")
	gitdir := mkdir(t, root, ".git", "worktrees", "wt")
	write(t, filepath.Join(gitdir, "commondir"), "../..\n")
	wt := mkdir(t, root, ".wt", "wt")
	write(t, filepath.Join(wt, ".git"), "gitdir: "+gitdir+"\n")
	if got, remote := LocateCheckout("")(wt); got != root || remote != "example.com/you/pkg" {
		t.Errorf("worktree: root=%q remote=%q, want %q and its origin", got, remote, root)
	}
}

func TestScopeOf(t *testing.T) {
	evs := []events.Event{
		{Kind: events.UserPrompt, CWD: "/home/you/src/project"},
		{Kind: events.AssistantText, CWD: "/home/you/src/project/.worktree/x"},
		{Kind: events.AssistantText, CWD: "/home/you/src/project/.worktree/x"},
		{Kind: events.AssistantText, CWD: "/somewhere/else", Sidechain: true},
		{Kind: events.AssistantText, CWD: "/somewhere/else", Sidechain: true},
		{Kind: events.AssistantText, CWD: "/somewhere/else", Sidechain: true},
	}
	locate := func(dir string) (string, string) {
		if !strings.HasPrefix(dir, "/home/you/src/project") {
			t.Errorf("located %q, want the session's own dominant directory", dir)
		}
		return "/home/you/src/project", "example.com/you/project"
	}
	got := scopeOf(evs, "/home/you", locate)
	want := Scope{CWD: "~/src/project/.worktree/x", Repo: "~/src/project", Remote: "example.com/you/project"}
	if got != want {
		t.Errorf("local scope = %+v, want %+v", got, want)
	}
	if got.Key() != "example.com/you/project" || (Scope{Repo: "~/notes"}).Key() != "~/notes" {
		t.Error("a scope's key is its origin, or its path without one")
	}
	if ExpandScopePath(got.Repo, "/Users/you") != "/Users/you/src/project" {
		t.Error("a scope path should expand under this machine's home")
	}

	// Another host's session keeps its directory, without its user name,
	// and is never looked up on this machine.
	foreign := scopeOf([]events.Event{{Kind: events.UserPrompt, CWD: "/Users/them/src/project"}}, "", nil)
	if foreign != (Scope{CWD: "~/src/project"}) {
		t.Errorf("foreign scope = %+v", foreign)
	}
	if (scopeOf(nil, "/home/you", locate) != Scope{}) {
		t.Error("a session with no directory has no scope")
	}
}

// An imported session's repo keys its scope with no checkout here.
func TestScopeOfRepo(t *testing.T) {
	evs := []events.Event{
		{Kind: events.UserPrompt, CWD: "/home/them/src/example", Repo: "example.com/you/example"},
		{Kind: events.AssistantText, CWD: "/home/them/src/example", Repo: "example.com/you/example"},
	}
	got := scopeOf(evs, "", nil)
	if want := (Scope{CWD: "~/src/example", Remote: "example.com/you/example"}); got != want {
		t.Errorf("scope = %+v, want %+v", got, want)
	}
	if got.Key() != "example.com/you/example" {
		t.Errorf("key = %q, want the event's repo", got.Key())
	}
	noCWD := scopeOf([]events.Event{{Kind: events.UserPrompt, Repo: "example.com/you/example"}}, "", nil)
	if noCWD != (Scope{Remote: "example.com/you/example"}) {
		t.Errorf("scope with no directory = %+v", noCWD)
	}
	none := func(string) (string, string) { return "", "" }
	if got := scopeOf(evs, "/home/you", none); got.Key() != "example.com/you/example" || got.Repo != "" {
		t.Errorf("scope with no checkout found = %+v", got)
	}
}

func TestMapRecordsScope(t *testing.T) {
	fake := &model.Fake{Response: "NONE\n"}
	m := mapper(store.NewMem(), fake, nil)
	m.Home = "/home/you"
	m.Locate = func(string) (string, string) { return "/home/you/src/project", "example.com/you/project" }
	evs := []events.Event{{Kind: events.UserPrompt, Text: "go", CWD: "/home/you/src/project/cmd", Timestamp: "2026-11-01T00:00:00Z"}}
	res, err := m.MapSession(context.Background(), "claude", "s1", evs)
	if err != nil {
		t.Fatal(err)
	}
	want := Scope{CWD: "~/src/project/cmd", Repo: "~/src/project", Remote: "example.com/you/project"}
	if res.Observations.Scope != want {
		t.Errorf("scope = %+v, want %+v", res.Observations.Scope, want)
	}
}

func TestCandidatesNameTheirRepos(t *testing.T) {
	a := session("s1", "2026-11-01", zshA)
	a.Scope = Scope{Repo: "~/src/project", Remote: "example.com/you/project"}
	b := session("s2", "2026-11-03", zshB)
	b.Scope = Scope{Repo: "~/src/other"}
	cl := &CandidateLedger{}
	one, _ := Open(cl.Cluster(append(filler(), a), "2026-08-13", "2026-11-11"), nil)
	var sb strings.Builder
	renderCandidates(&sb, one, 0, false)
	if !strings.Contains(sb.String(), "; repo example.com/you/project)") {
		t.Errorf("a one-repo candidate should name its repo:\n%s", sb.String())
	}
	// A session whose place isn't known never borrows another's repo.
	unplaced := session("s3", "2026-11-04", zshB)
	mixed, _ := Open(cl.Cluster(append(filler(), a, unplaced), "2026-08-13", "2026-11-11"), nil)
	sb.Reset()
	renderCandidates(&sb, mixed, 0, false)
	if !strings.Contains(sb.String(), "; 1 of them in repo example.com/you/project, the rest in no known repo)") {
		t.Errorf("a partly placed candidate claims too much:\n%s", sb.String())
	}
	both, _ := Open(cl.Cluster(append(filler(), a, b), "2026-08-13", "2026-11-11"), nil)
	sb.Reset()
	renderCandidates(&sb, both, 0, false)
	for _, want := range []string{"; 2 repos)", "session s2 in ~/src/other:", "session s1 in example.com/you/project:"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("a candidate across repos misses %q:\n%s", want, sb.String())
		}
	}
}

func TestSignalsOf(t *testing.T) {
	call := func(tool, input string) events.Event {
		return events.Event{Kind: events.ToolCall, Tool: tool, Input: input, CWD: "/home/you/src/project/.worktree/x"}
	}
	evs := []events.Event{
		call("Read", "/home/you/src/project/.worktree/x/README.md"),
		call("Bash", "ls /home/you/src/project/.worktree/x/internal 2>/dev/null && go test ./... | tail -5"),
		call("Bash", "go test ./..."), // not a look around
		call("Bash", "cat README.md docs/design.md; head -20 go.mod | wc -l"),
		call("Read", "/home/you/src/project/.worktree/x/README.md"),
		call("Grep", "func main in /home/you/src/project/.worktree/x/cmd"),
		call("Read", "/home/you/.config/tool/config.toml"),
		call("Edit", "/home/you/src/project/.worktree/x/README.md"),
		{Kind: events.ToolCall, Tool: "Read", Input: "/elsewhere", Sidechain: true},
	}
	got := signalsOf(evs, "/home/you")
	want := []Signal{
		{"Read", "README.md"}, {"Bash", "ls internal"}, {"Read", "docs/design.md"}, {"Read", "go.mod"},
		{"Grep", "func main in cmd"}, {"Read", "~/.config/tool/config.toml"},
	}
	if len(got) != len(want) {
		t.Fatalf("signals = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("signal %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if s := placeless("cat /Users/them/src/a.txt", "", ""); s != "cat ~/src/a.txt" {
		t.Errorf("another machine's home survived: %q", s)
	}
	var many []events.Event
	for i := 0; i < openingActions+5; i++ {
		many = append(many, call("Read", strings.Repeat("a", i+1)))
	}
	if n := len(signalsOf(many, "")); n != openingActions {
		t.Errorf("kept %d opening actions, want the first %d", n, openingActions)
	}
}

// placeless strips a directory only on a path-separator boundary: cwd /a/b
// must not rewrite /a/bc or /a/b.txt, nor home /data/u rewrite /data/u2.
func TestPlacelessBoundary(t *testing.T) {
	for _, tc := range []struct{ input, cwd, home, want string }{
		{"grep x /a/bc/f", "/a/b", "", "grep x /a/bc/f"},
		{"cat /a/b.txt", "/a/b", "", "cat /a/b.txt"},
		{"cat /a/b/f.txt", "/a/b", "", "cat f.txt"},
		{"ls /a/b", "/a/b", "", "ls ."},
		{"cat /data/u2/f", "", "/data/u", "cat /data/u2/f"},
		{"cat /data/u/f", "", "/data/u", "cat ~/f"},
		{"ls /data/u", "", "/data/u", "ls ~"},
	} {
		if got := placeless(tc.input, tc.cwd, tc.home); got != tc.want {
			t.Errorf("placeless(%q, %q, %q) = %q, want %q", tc.input, tc.cwd, tc.home, got, tc.want)
		}
	}
}

// A signal is stored and counted across sessions, so a credential in a
// search pattern or a listing command must not reach the store. The token is
// assembled at run time so no secret-shaped string sits in the repo.
func TestSignalsAreRedacted(t *testing.T) {
	tok := "gh" + "p_" + strings.Repeat("a1B2", 9)
	call := func(tool, input string) events.Event {
		return events.Event{Kind: events.ToolCall, Tool: tool, Input: input}
	}
	evs := []events.Event{
		call("Grep", tok+" in /home/you/src"),
		call("Bash", "grep "+tok+" ."),
	}
	got := signalsOf(evs, "/home/you")
	if len(got) != 2 {
		t.Fatalf("signals = %+v", got)
	}
	for _, s := range got {
		if strings.Contains(s.Input, tok) {
			t.Errorf("a secret survived in signal %+v", s)
		}
	}
	if want := "[REDACTED:github-token] in ~/src"; got[0].Input != want {
		t.Errorf("signal input = %q, want %q", got[0].Input, want)
	}
}

func TestRepeatsAcrossSessions(t *testing.T) {
	readme, main := Signal{"Read", "README.md"}, Signal{"Read", "cmd/main.go"}
	in := func(id, repo string, prompts int, signals ...Signal) SessionObservations {
		so := session(id, "2026-11-01")
		so.Scope, so.Prompts, so.Signals = Scope{Remote: repo}, prompts, signals
		return so
	}
	window := []SessionObservations{
		in("a", "example.com/you/project", 2, readme, main),
		in("b", "example.com/you/project", 1, readme),
		in("c", "example.com/you/project", 3, readme, main),
		in("d", "example.com/you/project", 1),
		in("scripted", "example.com/you/project", 0, readme, main), // no owner prompts: not counted
		in("x", "example.com/you/other", 1, readme),
		in("y", "example.com/you/other", 1, readme), // two sessions aren't a repeat
		in("old", "example.com/you/project", 1, readme, main),
	}
	window[len(window)-1].LastEvent = "2026-01-01T00:00:00Z"
	got := withRepeats(window, "2026-08-13", 0)
	want := "Sessions in example.com/you/project keep starting with the same look around: Read `README.md` (3 of 4)."
	for i, id := range []string{"a", "b", "c"} {
		if obs := got[i].Observations; len(obs) != 1 || obs[0].Category != CategoryRepeat || obs[0].Source != SourceRepeat || obs[0].Text != want {
			t.Errorf("session %s: observations = %+v", id, obs)
		}
	}
	for i := 3; i < len(got); i++ {
		if len(got[i].Observations) != 0 {
			t.Errorf("session %s took part in no repeat but got %+v", got[i].Session, got[i].Observations)
		}
	}
	if len(window[0].Observations) != 0 {
		t.Error("the caller's records were changed")
	}

	// The reduce reads it as one candidate counted across the sessions.
	cl := &CandidateLedger{}
	open, _ := Open(cl.Cluster(got, "2026-08-13", "2026-11-11"), nil)
	if len(open) != 1 || open[0].Sessions != 3 || open[0].Category != CategoryRepeat {
		t.Errorf("clusters = %+v", open)
	}
}

func TestHarnessScope(t *testing.T) {
	home := review.Resolve(t.TempDir())
	base := filepath.Join(home, ".agents", "base.md")
	claudeMD := filepath.Join(home, ".claude", "CLAUDE.md")
	steering := filepath.Join(home, ".kiro", "steering", "AGENTS.md")
	local := filepath.Join(home, "src", "project", "AGENTS.md")
	repo := Repo{Root: filepath.Join(home, "src", "project"), Remote: "example.com/you/project", Files: []RepoFile{{Path: local}}}
	both := map[string][]string{"claude": {claudeMD, base}, "kiro": {steering, base}, "chat": {}}
	kiroOnly := map[string][]string{"kiro": {steering, base}}
	own := map[string]string{"claude": claudeMD, "kiro": steering}
	e := RunEvidence{
		"c-kiro":   {{Session: "s1", Harness: "kiro"}},
		"c-claude": {{Session: "s2", Harness: "claude"}},
		"c-chat":   {{Session: "s3", Harness: "chat"}},
		"c-review": {{Session: "pr-1", Harness: "evidence-github"}},
		"c-old":    {{Session: "s4"}},
		"c-repo":   {{Session: "s5", Harness: "kiro", Repo: "example.com/you/project"}},
	}
	add := func(cands ...string) Proposal { return Proposal{Kind: "add", Candidates: cands} }
	cases := []struct {
		name    string
		p       Proposal
		target  string
		readers map[string][]string
		want    string
	}{
		{"a kiro-only add aimed at a base file both load", add("c-kiro"), base, both, "it belongs in ~/.kiro/steering/AGENTS.md"},
		{"the same add with only kiro enabled", add("c-kiro"), base, kiroOnly, ""},
		{"a kiro-only add aimed at kiro's own file", add("c-kiro"), steering, both, ""},
		{"a kiro-only add aimed at claude's file", add("c-kiro"), claudeMD, both, "claude loads ~/.claude/CLAUDE.md too"},
		{"an add only an import's sessions support", add("c-chat"), claudeMD, both, "loads no context file of its own"},
		{"an add citing an import and a local harness", add("c-chat", "c-claude"), base, both, ""},
		{"outside evidence doesn't widen a one-harness add", add("c-kiro", "c-review"), base, both, "came from kiro"},
		{"outside evidence doesn't narrow a shared add", add("c-kiro", "c-claude", "c-review"), base, both, ""},
		{"outside evidence alone", add("c-review"), base, both, ""},
		{"evidence recorded before refs named a harness", add("c-kiro", "c-old"), base, both, ""},
		{"a reword stays where its rule lives", Proposal{Kind: "reword", Candidates: []string{"c-kiro"}}, base, both, ""},
		{"repo scope applies first", add("c-repo"), local, both, ""},
	}
	for _, c := range cases {
		got := harnessScope(c.p, c.target, []Repo{repo}, e, c.readers, own, home)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: harnessScope = %q, want %q", c.name, got, c.want)
		}
	}
	// A harness whose default file another harness loads too gets pointed
	// at a file only it loads.
	shared := map[string][]string{"claude": {claudeMD, steering}, "kiro": {steering}}
	if got := harnessScope(add("c-kiro"), steering, nil, e, shared, own, home); !strings.Contains(got, "it belongs in a file only kiro loads") {
		t.Errorf("shared own file = %q", got)
	}
}
