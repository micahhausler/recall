package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolate keeps git away from the owner's global config (signing, hooks)
// and gives commits an identity.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "you")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "you@example.com")
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// clone makes a bare origin with one commit on main and returns a clone.
func clone(t *testing.T) string {
	t.Helper()
	isolate(t)
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	run(t, base, "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(base, "seed")
	run(t, base, "clone", "-q", origin, seed)
	if err := os.WriteFile(filepath.Join(seed, "CLAUDE.md"), []byte("# rules\n\n- one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, seed, "add", "-A")
	run(t, seed, "commit", "-q", "-m", "seed")
	run(t, seed, "push", "-q", "origin", "HEAD:main")
	live := filepath.Join(base, "live")
	run(t, base, "clone", "-q", origin, live)
	return Resolve(live)
}

func TestRepoRoot(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	for _, p := range []string{live, filepath.Join(live, "CLAUDE.md"), filepath.Join(live, "skills", "new", "SKILL.md")} {
		if root, ok := RepoRoot(ctx, p); !ok || root != live {
			t.Errorf("RepoRoot(%s) = %q, %v; want %s", p, root, ok, live)
		}
	}
	if _, ok := RepoRoot(ctx, filepath.Join(t.TempDir(), "x.md")); ok {
		t.Error("a path outside any checkout has a repo root")
	}
}

func TestWorktreeLifecycle(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	// A local commit the remote doesn't have must not reach the branch.
	if err := os.WriteFile(filepath.Join(live, "local.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, live, "add", "-A")
	run(t, live, "commit", "-q", "-m", "local only")

	w, err := AddWorktree(ctx, live, filepath.Join(t.TempDir(), "wt"), "learn-2026-11-11")
	if err != nil {
		t.Fatal(err)
	}
	if w.Remote != "origin" || w.Base != "origin/main" || w.BaseBranch() != "main" {
		t.Errorf("worktree = %+v, want origin/main as the base", w)
	}
	if _, err := os.Stat(filepath.Join(w.Dir, "local.md")); err == nil {
		t.Error("the branch started from the local HEAD, not origin's default branch")
	}
	if changed, err := w.Changed(ctx); err != nil || len(changed) != 0 {
		t.Errorf("fresh worktree changed = %v, %v", changed, err)
	}

	if err := os.WriteFile(filepath.Join(w.Dir, "CLAUDE.md"), []byte("# rules\n\n- one\n- two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(w.Dir, "skills", "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "skills", "new", "SKILL.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := w.Changed(ctx)
	if err != nil || strings.Join(changed, ",") != "CLAUDE.md,skills/new/SKILL.md" {
		t.Errorf("changed = %v, %v", changed, err)
	}
	diff, err := w.Stage(ctx)
	if err != nil || !strings.Contains(diff, "+- two") {
		t.Errorf("staged diff = %q, %v", diff, err)
	}
	if err := w.Commit(ctx, "Apply learn proposals", "- applied add: two"); err != nil {
		t.Fatal(err)
	}
	if msg := run(t, live, "log", "-1", "--format=%B", w.Branch); !strings.HasPrefix(msg, "Apply learn proposals\n\n- applied add: two") {
		t.Errorf("commit message = %q", msg)
	}

	// The same branch again is an earlier apply waiting for review.
	if _, err := AddWorktree(ctx, live, filepath.Join(t.TempDir(), "wt2"), w.Branch); err == nil {
		t.Error("a second worktree reused an existing branch")
	}

	if err := w.Remove(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Dir); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists: %v", err)
	}
	run(t, live, "rev-parse", "--verify", "refs/heads/"+w.Branch) // kept
}

func TestRemoveDropsBranch(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	w, err := AddWorktree(ctx, live, filepath.Join(t.TempDir(), "wt"), "learn-x")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Remove(ctx, false); err != nil {
		t.Fatal(err)
	}
	if exists(ctx, live, "refs/heads/learn-x") {
		t.Error("branch kept")
	}
}

func TestNoOriginBranchesFromHEAD(t *testing.T) {
	ctx := context.Background()
	isolate(t)
	repo := t.TempDir()
	run(t, repo, "init", "-q", "-b", "trunk")
	run(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	w, err := AddWorktree(ctx, Resolve(repo), filepath.Join(t.TempDir(), "wt"), "learn-x")
	if err != nil {
		t.Fatal(err)
	}
	if w.Remote != "" || w.Base != "HEAD" {
		t.Errorf("worktree = %+v, want HEAD with no remote", w)
	}
	if _, err := (&PR{}).Publish(ctx, Change{Worktree: w}); err == nil || !strings.Contains(err.Error(), "learn.review") {
		t.Errorf("PR without origin: %v, want a hint to use branch review", err)
	}
	got, err := Branch{}.Publish(ctx, Change{Worktree: w})
	if err != nil || !strings.Contains(got, "learn-x") {
		t.Errorf("branch publish = %q, %v", got, err)
	}
}

func TestPRPushesAndOpens(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	w, err := AddWorktree(ctx, live, filepath.Join(t.TempDir(), "wt"), "learn-x")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "CLAUDE.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Stage(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(ctx, "t", ""); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	gh := filepath.Join(bin, "gh")
	script := "#!/bin/sh\necho \"$*\" > " + argsFile + "\ncat > " + argsFile + ".body\necho https://example.com/pull/1\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	url, err := (&PR{GH: gh}).Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "the body"})
	if err != nil || url != "https://example.com/pull/1" {
		t.Fatalf("publish = %q, %v", url, err)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--base main --head learn-x --title Apply") {
		t.Errorf("gh args = %s", args)
	}
	if body, _ := os.ReadFile(argsFile + ".body"); string(body) != "the body" {
		t.Errorf("gh body = %q", body)
	}
	run(t, live, "fetch", "-q", "origin")
	run(t, live, "rev-parse", "--verify", "refs/remotes/origin/learn-x") // pushed
}

func TestNew(t *testing.T) {
	for _, n := range Names() {
		if _, err := New(n); err != nil {
			t.Errorf("New(%q): %v", n, err)
		}
	}
	if _, err := New("direct"); err == nil {
		t.Error("New accepted an unregistered name")
	}
}

func TestPRVerdict(t *testing.T) {
	ctx := context.Background()
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	// The URL picks the state, so one script answers all three.
	script := `#!/bin/sh
case "$3" in
*/1) echo '{"state":"OPEN","mergedAt":null,"closedAt":null}' ;;
*/2) echo '{"state":"MERGED","mergedAt":"2026-11-13T10:00:00Z","closedAt":"2026-11-13T10:00:00Z"}' ;;
*/3) echo '{"state":"CLOSED","mergedAt":null,"closedAt":"2026-11-14T09:00:00Z"}' ;;
*) echo 'no such pull request' >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &PR{GH: gh}
	for url, want := range map[string]struct {
		v   Verdict
		day string
	}{
		"https://example.com/pull/1": {Pending, ""},
		"https://example.com/pull/2": {Merged, "2026-11-13"},
		"https://example.com/pull/3": {Closed, "2026-11-14"},
	} {
		v, d, err := p.Verdict(ctx, Published{Where: url})
		if err != nil || v != want.v || d != want.day {
			t.Errorf("%s: %s %s %v, want %s %s", url, v, d, err, want.v, want.day)
		}
	}
	if _, _, err := p.Verdict(ctx, Published{Where: "https://example.com/pull/9"}); err == nil ||
		!strings.Contains(err.Error(), "no such pull request") {
		t.Errorf("missing PR err = %v", err)
	}
}

func TestBranchVerdict(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	commit := func(branch string) Published {
		w, err := AddWorktree(ctx, live, filepath.Join(t.TempDir(), "wt"), branch)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(w.Dir, branch+".md"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Stage(ctx); err != nil {
			t.Fatal(err)
		}
		if err := w.Commit(ctx, branch, ""); err != nil {
			t.Fatal(err)
		}
		head, err := w.Head(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Remove(ctx, true); err != nil {
			t.Fatal(err)
		}
		return Published{Repo: live, Branch: branch, Base: w.Base, Commit: head}
	}
	kept, merged, dropped := commit("learn-a"), commit("learn-b"), commit("learn-c")
	run(t, live, "merge", "-q", "--no-edit", "learn-b") // into the local main
	run(t, live, "branch", "-q", "-D", "learn-c")
	for _, tc := range []struct {
		p    Published
		want Verdict
	}{{kept, Pending}, {merged, Merged}, {dropped, Closed}} {
		if v, _, err := (Branch{}).Verdict(ctx, tc.p); err != nil || v != tc.want {
			t.Errorf("%s: %s, %v; want %s", tc.p.Branch, v, err, tc.want)
		}
	}
}
