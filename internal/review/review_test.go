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
	diff, err := w.Stage(ctx, changed)
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
	if _, err := w.Stage(ctx, []string{"CLAUDE.md"}); err != nil {
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
	if strings.Contains(string(args), "--repo") {
		t.Errorf("an origin that names no host has no repo to pass: %s", args)
	}
	if body, _ := os.ReadFile(argsFile + ".body"); string(body) != "the body" {
		t.Errorf("gh body = %q", body)
	}
	run(t, live, "fetch", "-q", "origin")
	run(t, live, "rev-parse", "--verify", "refs/remotes/origin/learn-x") // pushed
}

// In a fork, gh would aim a pull request at the repo the fork came from.
// The publisher names origin, so the change stays on the owner's own repo.
func TestPROpensOnOrigin(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	w, err := AddWorktree(ctx, live, filepath.Join(t.TempDir(), "wt"), "learn-y")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "CLAUDE.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Stage(ctx, []string{"CLAUDE.md"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(ctx, "t", ""); err != nil {
		t.Fatal(err)
	}
	// Fetches read the hosted URL; pushes still go to the local bare repo.
	bare := run(t, live, "remote", "get-url", "origin")
	run(t, live, "remote", "set-url", "origin", "git@github.com:you/dotfiles.git")
	run(t, live, "remote", "set-url", "--push", "origin", bare)
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	gh := filepath.Join(bin, "gh")
	script := "#!/bin/sh\necho \"$*\" > " + argsFile + "\ncat > /dev/null\necho https://example.com/pull/2\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (&PR{GH: gh}).Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if args, _ := os.ReadFile(argsFile); !strings.Contains(string(args), "--repo you/dotfiles") {
		t.Errorf("gh args = %s, want the pull request opened on origin", args)
	}
}

// gh failing after the push must not leave the branch on origin, where it
// would block the next apply until the owner deletes it by hand.
func TestPRDeletesPushedBranchWhenGHFails(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	w, err := AddWorktree(ctx, live, filepath.Join(t.TempDir(), "wt"), "learn-x")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "CLAUDE.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Stage(ctx, []string{"CLAUDE.md"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(ctx, "t", ""); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	script := "#!/bin/sh\necho 'gh: not signed in' >&2\nexit 1\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = (&PR{GH: gh}).Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "not signed in") || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("publish err = %v, want gh's failure plus what became of the pushed branch", err)
	}
	run(t, live, "fetch", "-q", "--prune", "origin")
	if exists(ctx, live, "refs/remotes/origin/learn-x") {
		t.Error("the pushed branch was left on origin")
	}
}

// A branch already on origin is an earlier apply's, waiting for review: the
// worktree refuses before anything is pushed, and the branch stays.
func TestPreexistingRemoteBranchRefusedAndKept(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	run(t, live, "push", "-q", "origin", "HEAD:refs/heads/learn-x")
	if _, err := AddWorktree(ctx, live, filepath.Join(t.TempDir(), "wt"), "learn-x"); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want the existing-branch refusal", err)
	}
	run(t, live, "fetch", "-q", "--prune", "origin")
	if !exists(ctx, live, "refs/remotes/origin/learn-x") {
		t.Error("the pre-existing remote branch was deleted")
	}
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
		if _, err := w.Stage(ctx, []string{branch + ".md"}); err != nil {
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

// RuleLanded must not read a discarded proposal as merged because its short
// text was already somewhere in the base, and must read a proposal whose
// own commit is an ancestor of the base as landed whatever the wording
// became.
func TestRuleLandedAncestryAndExactBullet(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	// The base holds "one" as a bullet, and the short rule text only inside
	// a longer line.
	if err := os.WriteFile(filepath.Join(live, "CLAUDE.md"),
		[]byte("# rules\n\n- one\n- Run tests. Then run vet.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, live, "add", "-A")
	run(t, live, "commit", "-q", "-m", "landed")
	head := run(t, live, "rev-parse", "HEAD")
	run(t, live, "push", "-q", "origin", "HEAD:main")
	// A commit the owner dropped before merging: on a branch, never in main.
	run(t, live, "checkout", "-q", "-b", "learn-x")
	if err := os.WriteFile(filepath.Join(live, "extra.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, live, "add", "-A")
	run(t, live, "commit", "-q", "-m", "dropped")
	dropped := run(t, live, "rev-parse", "HEAD")
	run(t, live, "checkout", "-q", "main")

	p := Published{Repo: live, Base: "origin/main", Branch: "learn-x", Commit: dropped}
	// A short rule whose text pre-existed in the base, inside a longer
	// line, is not landed when its commit is absent.
	if landed, ok := RuleLanded(ctx, p, "Run tests.", MergeRange{}); landed || !ok {
		t.Errorf("short pre-existing text: landed=%v ok=%v, want false true", landed, ok)
	}
	// The exact whole bullet line still lands.
	if landed, ok := RuleLanded(ctx, p, "one", MergeRange{}); !landed || !ok {
		t.Errorf("exact bullet: landed=%v ok=%v, want true true", landed, ok)
	}
	// An ancestor commit landed, even reworded past recognition.
	p.Commit = head
	if landed, ok := RuleLanded(ctx, p, "Totally reworded before merging.", MergeRange{}); !landed || !ok {
		t.Errorf("ancestor commit: landed=%v ok=%v, want true true", landed, ok)
	}
}

// A proposal the owner reworded by hand, then merged by rebase or squash,
// landed; a reword the owner rejected, leaving the old rule in place, did
// not, though the old rule reads much like the proposal.
func TestRuleLandedEditedByHand(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(live, "CLAUDE.md"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := "- Use SSH for remotes, and stop if SSH auth fails on a machine.\n"
	write("# rules\n\n" + old)
	run(t, live, "add", "-A")
	run(t, live, "commit", "-q", "-m", "base")
	run(t, live, "push", "-q", "origin", "HEAD:main")

	// The proposal's own commit, on a branch the merge rewrites away.
	proposed := "Use SSH for GitHub remotes. If SSH auth fails on a machine, stop and help me set up SSH there; don't switch the remote or scripts to HTTPS."
	run(t, live, "checkout", "-q", "-b", "learn-x")
	write("# rules\n\n- " + proposed + "\n")
	run(t, live, "commit", "-q", "-am", "reword")
	commit := run(t, live, "rev-parse", "HEAD")
	run(t, live, "checkout", "-q", "main")
	p := Published{Repo: live, Base: "origin/main", Branch: "learn-x", Commit: commit}

	// Rejected: main still holds the old rule.
	if landed, _ := RuleLanded(ctx, p, proposed, MergeRange{}); landed {
		t.Error("rejected reword read as landed")
	}

	// Edited by hand, then merged as a new commit.
	write("# rules\n\n- Use SSH for `origin` when it's my repo or my fork. An `upstream` remote over HTTPS is fine. " +
		"If SSH auth fails on a machine, stop and help me set up SSH there; don't switch `origin` or scripts to HTTPS.\n")
	run(t, live, "commit", "-q", "-am", "squashed")
	run(t, live, "push", "-q", "origin", "HEAD:main")
	if landed, ok := RuleLanded(ctx, p, proposed, MergeRange{}); !landed || !ok {
		t.Errorf("hand-edited reword: landed=%v ok=%v, want true true", landed, ok)
	}

	// An unrelated new bullet is not the proposal.
	write("# rules\n\n" + old + "- Keep PR descriptions to a few short lines.\n")
	run(t, live, "commit", "-q", "-am", "other")
	run(t, live, "push", "-q", "origin", "HEAD:main")
	if landed, _ := RuleLanded(ctx, p, proposed, MergeRange{}); landed {
		t.Error("unrelated new bullet read as the proposal")
	}
}

// A rewording that keeps the proposal's words and adds many more landed:
// the match is measured against the proposal, so additions don't count
// against it.
func TestRuleLandedElaboratedByHand(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	name := "agent notes.md" // a space, which a whitespace split of git's output would break
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(live, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("# rules\n\n- one\n")
	run(t, live, "add", "-A")
	run(t, live, "commit", "-q", "-m", "base")
	run(t, live, "push", "-q", "origin", "HEAD:main")

	proposed := "Tear down test clusters and cloud resources in a background agent, then confirm nothing is left running anywhere."
	run(t, live, "checkout", "-q", "-b", "learn-x")
	write("# rules\n\n- one\n- " + proposed + "\n")
	run(t, live, "commit", "-q", "-am", "add")
	commit := run(t, live, "rev-parse", "HEAD")
	run(t, live, "checkout", "-q", "main")

	write("# rules\n\n- one\n- Tear down e2e and test clusters, load balancers, and other cloud resources in a background agent " +
		"once a run finishes or fails, list what you deleted, then confirm nothing is left running anywhere in the account or region.\n")
	run(t, live, "commit", "-q", "-am", "squashed")
	run(t, live, "push", "-q", "origin", "HEAD:main")
	p := Published{Repo: live, Base: "origin/main", Branch: "learn-x", Commit: commit}
	if landed, ok := RuleLanded(ctx, p, proposed, MergeRange{}); !landed || !ok {
		t.Errorf("elaborated rewording: landed=%v ok=%v, want true true", landed, ok)
	}
}

// With a pull request's merge range, only what that merge added counts: a
// similar rule another change added later doesn't make a rejected proposal
// look landed, and the proposal's own commit isn't needed.
func TestRuleLandedScopedToTheMerge(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(live, "rules.md"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("# rules\n\n- one\n")
	run(t, live, "add", "-A")
	run(t, live, "commit", "-q", "-m", "base")
	proposed := "Use SSH for GitHub remotes. If SSH auth fails on a machine, stop and help me set up SSH there."

	// The pull request merged, as one squashed commit, without the proposal.
	write("# rules\n\n- one\n- Keep PR descriptions short.\n")
	run(t, live, "commit", "-q", "-am", "the merge")
	merge := run(t, live, "rev-parse", "HEAD")
	// Later, another change adds a rule close to the proposal.
	write("# rules\n\n- one\n- Keep PR descriptions short.\n- Use SSH for GitHub remotes; if SSH auth fails on a machine, stop and help me set up SSH there.\n")
	run(t, live, "commit", "-q", "-am", "the other side")
	run(t, live, "push", "-q", "origin", "HEAD:main")

	// No proposal commit at all: the range alone decides.
	p := Published{Repo: live, Base: "origin/main", Branch: "learn-x"}
	if landed, _ := RuleLanded(ctx, p, proposed, MergeRange{To: merge, Commits: 1}); landed {
		t.Error("a similar rule added after the merge read as the proposal landing")
	}
	// The same rule inside the merge's own range is the proposal, edited.
	later := run(t, live, "rev-parse", "HEAD")
	if landed, ok := RuleLanded(ctx, p, proposed, MergeRange{To: later, Commits: 1}); !landed || !ok {
		t.Errorf("edited rule in the merge's range: landed=%v ok=%v, want true true", landed, ok)
	}
}
