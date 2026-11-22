package review

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/micahhausler/recall/internal/credential"
)

const fakeToken = "ghs_fakeinstallationtoken"

// forge is a local stand-in for a git host: a bare repo served over smart
// HTTP by git's own http-backend, behind basic auth that accepts exactly
// one token the way GitHub accepts an installation token.
type forge struct {
	URL  string
	Bare string

	mu      sync.Mutex
	authed  int
	refused []string
}

func newForge(t *testing.T, seedFrom string) *forge {
	t.Helper()
	execPath := run(t, t.TempDir(), "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git http-backend isn't installed: %v", err)
	}
	root := t.TempDir()
	f := &forge{Bare: filepath.Join(root, "you", "dotfiles.git")}
	if err := os.MkdirAll(filepath.Dir(f.Bare), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, root, "clone", "-q", "--bare", seedFrom, f.Bare)
	run(t, f.Bare, "config", "http.receivepack", "true")
	git := &cgi.Handler{Path: backend, Dir: root,
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
			http.Error(w, "sign in", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		if user != "x-access-token" || pass != fakeToken {
			f.refused = append(f.refused, user+":"+pass)
			f.mu.Unlock()
			http.Error(w, "bad credentials", http.StatusForbidden)
			return
		}
		f.authed++
		f.mu.Unlock()
		git.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// traceGit puts a git on PATH that logs every invocation's arguments before
// running the real one, and returns the log's path.
func traceGit(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// fakeGHEnv is a gh that records each call's arguments and the token in its
// environment, and answers pr create with a pull request URL.
func fakeGHEnv(t *testing.T) (gh, log string) {
	t.Helper()
	dir := t.TempDir()
	gh, log = filepath.Join(dir, "gh"), filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"$GH_TOKEN\" \"$*\" >> '" + log + "'\n" +
		"case \"$1\" in pr) cat > /dev/null; echo https://github.com/you/dotfiles/pull/7;; esac\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return gh, log
}

// committed makes a worktree of live with one commit on branch.
func committed(t *testing.T, live, branch string) *Worktree {
	t.Helper()
	ctx := context.Background()
	w, err := AddWorktree(ctx, live, filepath.Join(t.TempDir(), "wt"), branch)
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
	return w
}

// With credentials, the push goes over HTTPS with the token, gh runs with
// the token, and the token appears in no argv, remote URL, or config file.
// The owner's own credential helper and URL rewrites must not get in the
// way, and the helper must never be handed the token to store.
func TestPRWithCredentialsPushesAndOpensAsThem(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	f := newForge(t, run(t, live, "remote", "get-url", "origin"))
	w := committed(t, live, "learn-x")
	// The checkout is cloned over SSH, as the owner's own repos are.
	run(t, live, "remote", "set-url", "origin", "git@github.com:you/dotfiles.git")

	// The owner's git config: a credential helper holding their own login,
	// and a rule sending every URL on the forge over SSH.
	helperLog := filepath.Join(t.TempDir(), "helper.log")
	global := os.Getenv("GIT_CONFIG_GLOBAL")
	run(t, live, "config", "--file", global, "credential.helper",
		"!f() { echo \"$1\" >> '"+helperLog+"'; echo username=owner; echo password=owners-own-login; }; f")
	run(t, live, "config", "--file", global, "url.ssh://git@unreachable.invalid/.insteadOf", f.URL+"/")
	run(t, live, "config", "--file", global, "url.ssh://git@unreachable.invalid/.pushInsteadOf", f.URL+"/")

	argv := traceGit(t)
	gh, ghLog := fakeGHEnv(t)
	creds := &credential.Fake{Credential: credential.Credential{Token: fakeToken, Owner: "you"}}
	var warn bytes.Buffer
	p := &PR{GH: gh, Credentials: creds, Log: &warn, base: f.URL}
	where, err := p.Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "b"})
	if err != nil || where != "https://github.com/you/dotfiles/pull/7" {
		t.Fatalf("publish = %q, %v", where, err)
	}
	if len(creds.Asked) == 0 || creds.Asked[0] != "github.com/you/dotfiles" {
		t.Errorf("token asked for %v, want the origin's slug", creds.Asked)
	}

	// The branch reached the forge, authenticated with the token.
	run(t, f.Bare, "rev-parse", "--verify", "refs/heads/learn-x")
	if f.authed == 0 || len(f.refused) > 0 {
		t.Errorf("forge: %d authenticated requests, refused %v", f.authed, f.refused)
	}
	if data, err := os.ReadFile(helperLog); err == nil {
		t.Errorf("the owner's credential helper ran (%q); it must be neither asked nor told to store the token", data)
	}

	// The token is in the environment only.
	args, _ := os.ReadFile(argv)
	if strings.Contains(string(args), fakeToken) {
		t.Errorf("token in a git argv:\n%s", args)
	}
	if !strings.Contains(string(args), "push -q "+f.URL+"/you/dotfiles.git refs/heads/learn-x:refs/heads/learn-x") {
		t.Errorf("git argv lacks the HTTPS push:\n%s", args)
	}
	for _, cfg := range []string{filepath.Join(live, ".git", "config"), global} {
		if data, _ := os.ReadFile(cfg); strings.Contains(string(data), fakeToken) {
			t.Errorf("token written to %s", cfg)
		}
	}
	if got := run(t, live, "remote", "get-url", "origin"); got != "git@github.com:you/dotfiles.git" {
		t.Errorf("origin rewritten to %s", got)
	}

	// gh ran as the credentials: it opened the pull request on origin,
	// then asked the credential's owner to review it.
	calls, _ := os.ReadFile(ghLog)
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 {
		t.Fatalf("gh calls = %q, want pr create then the review request", lines)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, fakeToken+"|") {
			t.Errorf("gh ran without the token: %s", l)
		}
		if strings.Contains(strings.TrimPrefix(l, fakeToken+"|"), fakeToken) {
			t.Errorf("token in gh argv: %s", l)
		}
	}
	if !strings.Contains(lines[0], "pr create") || !strings.Contains(lines[0], "--repo you/dotfiles") {
		t.Errorf("first gh call = %s", lines[0])
	}
	if !strings.Contains(lines[1], "api --method POST --silent repos/you/dotfiles/pulls/7/requested_reviewers -f reviewers[]=you") {
		t.Errorf("review request = %s", lines[1])
	}
	if warn.Len() > 0 {
		t.Errorf("warned: %s", warn.String())
	}
}

// A credential that names no owner opens the pull request and asks no one.
func TestPRWithCredentialsAndNoOwnerRequestsNoReview(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	f := newForge(t, run(t, live, "remote", "get-url", "origin"))
	w := committed(t, live, "learn-x")
	run(t, live, "remote", "set-url", "origin", "git@github.com:you/dotfiles.git")
	gh, ghLog := fakeGHEnv(t)
	p := &PR{GH: gh, Credentials: &credential.Fake{Credential: credential.Credential{Token: fakeToken}}, base: f.URL}
	if _, err := p.Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(ghLog)
	if lines := strings.Split(strings.TrimSpace(string(calls)), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "pr create") {
		t.Errorf("gh calls = %q, want only pr create", lines)
	}
}

// A refused review request leaves the pull request published, with a
// warning that names it.
func TestPRReviewRequestFailureIsAWarning(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	f := newForge(t, run(t, live, "remote", "get-url", "origin"))
	w := committed(t, live, "learn-x")
	run(t, live, "remote", "set-url", "origin", "git@github.com:you/dotfiles.git")
	gh := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\ncase \"$1\" in pr) cat > /dev/null; echo https://github.com/you/dotfiles/pull/7;; " +
		"*) echo 'Reviews may only be requested from collaborators' >&2; exit 1;; esac\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	p := &PR{GH: gh, Credentials: &credential.Fake{Credential: credential.Credential{Token: fakeToken, Owner: "you"}}, Log: &warn, base: f.URL}
	where, err := p.Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "b"})
	if err != nil || where != "https://github.com/you/dotfiles/pull/7" {
		t.Fatalf("publish = %q, %v; a refused review request must not fail the change", where, err)
	}
	if !strings.Contains(warn.String(), "collaborators") || !strings.Contains(warn.String(), where) || !strings.Contains(warn.String(), "asking you to review") {
		t.Errorf("warning = %q", warn.String())
	}
	run(t, f.Bare, "rev-parse", "--verify", "refs/heads/learn-x") // the branch stays
}

// assigningGH is a gh that logs each call with its token, opens pull
// request 7, and answers an assignment with the logins in assigned, one per
// line, as gh's --jq prints them.
func assigningGH(t *testing.T, assigned string) (gh, log string) {
	t.Helper()
	dir := t.TempDir()
	gh, log = filepath.Join(dir, "gh"), filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"$GH_TOKEN\" \"$*\" >> '" + log + "'\n" +
		"case \"$*\" in pr\\ create*) cat > /dev/null; echo https://github.com/you/dotfiles/pull/7;; " +
		"*/assignees*) printf '" + assigned + "';; esac\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return gh, log
}

// review.assignees are assigned once the pull request is open, as the
// credentials, after the review request.
func TestPRAssignsThePullRequest(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	f := newForge(t, run(t, live, "remote", "get-url", "origin"))
	w := committed(t, live, "learn-x")
	run(t, live, "remote", "set-url", "origin", "git@github.com:you/dotfiles.git")
	gh, ghLog := assigningGH(t, `You\nteammate\n`)
	var warn bytes.Buffer
	p := &PR{GH: gh, Credentials: &credential.Fake{Credential: credential.Credential{Token: fakeToken, Owner: "you"}},
		Assignees: []string{"you", "teammate"}, Log: &warn, base: f.URL}
	where, err := p.Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "b"})
	if err != nil || where != "https://github.com/you/dotfiles/pull/7" {
		t.Fatalf("publish = %q, %v", where, err)
	}
	calls, _ := os.ReadFile(ghLog)
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 3 {
		t.Fatalf("gh calls = %q, want pr create, the review request, then the assignment", lines)
	}
	want := fakeToken + "|api --method POST --jq .assignees[].login repos/you/dotfiles/issues/7/assignees -f assignees[]=you -f assignees[]=teammate"
	if lines[2] != want {
		t.Errorf("assignment = %s\nwant         %s", lines[2], want)
	}
	// The forge spells a login its own way; a match ignores case.
	if warn.Len() > 0 {
		t.Errorf("warned: %s", warn.String())
	}
}

// Without credentials the assignment goes out as the machine's own login.
func TestPRAssignsWithoutCredentials(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	w := committed(t, live, "learn-x")
	gh, ghLog := assigningGH(t, `you\n`)
	var warn bytes.Buffer
	t.Setenv("GH_TOKEN", "")
	where, err := (&PR{GH: gh, Assignees: []string{"you"}, Log: &warn}).Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "b"})
	if err != nil || where != "https://github.com/you/dotfiles/pull/7" {
		t.Fatalf("publish = %q, %v", where, err)
	}
	calls, _ := os.ReadFile(ghLog)
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 || lines[1] != "|api --method POST --jq .assignees[].login repos/you/dotfiles/issues/7/assignees -f assignees[]=you" {
		t.Errorf("gh calls = %q, want pr create then the assignment, with no token", lines)
	}
	if warn.Len() > 0 {
		t.Errorf("warned: %s", warn.String())
	}

	// With no assignees, nothing is asked.
	gh, ghLog = assigningGH(t, ``)
	w = committed(t, clone(t), "learn-y")
	if _, err := (&PR{GH: gh}).Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	calls, _ = os.ReadFile(ghLog)
	if lines := strings.Split(strings.TrimSpace(string(calls)), "\n"); len(lines) != 1 {
		t.Errorf("gh calls = %q, want only pr create", lines)
	}
}

// The forge answers a login it can't assign with success and leaves it out.
// That, and an assignment it refuses outright, are each a warning that
// names who wasn't assigned; the pull request stays published.
func TestPRAssignmentThatDidNotTakeIsAWarning(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct{ script, want string }{
		"left out": {"*/assignees*) echo you;; ", "didn't assign teammate"},
		"refused":  {"*/assignees*) echo 'Resource not accessible by integration' >&2; exit 1;; ", "Resource not accessible by integration"},
	} {
		live := clone(t)
		w := committed(t, live, "learn-x")
		gh := filepath.Join(t.TempDir(), "gh")
		script := "#!/bin/sh\ncase \"$*\" in pr\\ create*) cat > /dev/null; echo https://github.com/you/dotfiles/pull/7;; " + tc.script + "esac\n"
		if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		var warn bytes.Buffer
		where, err := (&PR{GH: gh, Assignees: []string{"you", "teammate"}, Log: &warn}).Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "b"})
		if err != nil || where != "https://github.com/you/dotfiles/pull/7" {
			t.Fatalf("%s: publish = %q, %v; an assignment that didn't take must not fail the change", name, where, err)
		}
		if got := warn.String(); !strings.Contains(got, tc.want) || !strings.Contains(got, where) || !strings.Contains(got, "assigning it to you, teammate failed") {
			t.Errorf("%s: warning = %q, want %q", name, got, tc.want)
		}
		run(t, live, "fetch", "-q", "origin")
		run(t, live, "rev-parse", "--verify", "refs/remotes/origin/learn-x") // the branch stays
	}
}

// When gh fails, the branch the credentials pushed is taken back with them.
func TestPRWithCredentialsDeletesPushedBranchWhenGHFails(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	f := newForge(t, run(t, live, "remote", "get-url", "origin"))
	w := committed(t, live, "learn-x")
	run(t, live, "remote", "set-url", "origin", "https://github.com/you/dotfiles.git")
	gh := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\necho 'gh: refused' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &PR{GH: gh, Credentials: &credential.Fake{Credential: credential.Credential{Token: fakeToken}}, base: f.URL}
	_, err := p.Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "was deleted") {
		t.Fatalf("publish err = %v, want gh's failure and the branch taken back", err)
	}
	if out, err := exec.Command("git", "-C", f.Bare, "rev-parse", "--verify", "--quiet", "refs/heads/learn-x").Output(); err == nil {
		t.Errorf("the pushed branch was left on the forge at %s", out)
	}
}

// Credentials that don't cover the repo stop the publish before anything
// is pushed: the publisher never falls back to the machine's own login.
func TestPRWithCredentialsRefusesUncoveredRepo(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	bare := run(t, live, "remote", "get-url", "origin")
	w := committed(t, live, "learn-x")
	run(t, live, "remote", "set-url", "origin", "git@github.com:you/dotfiles.git")
	run(t, live, "remote", "set-url", "--push", "origin", bare) // where a fallback would land
	gh, ghLog := fakeGHEnv(t)
	p := &PR{GH: gh, Credentials: &credential.Fake{Err: errors.New("the app isn't installed on you/dotfiles")}}
	_, err := p.Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "isn't installed") {
		t.Fatalf("publish err = %v, want the credentials' refusal", err)
	}
	if exists(ctx, bare, "refs/heads/learn-x") {
		t.Error("the branch was pushed as the machine's own login")
	}
	if _, err := os.Stat(ghLog); err == nil {
		t.Error("gh ran without credentials for the repo")
	}
}

func TestPRWithCredentialsNeedsHostedOrigin(t *testing.T) {
	ctx := context.Background()
	live := clone(t) // origin is a local path
	w := committed(t, live, "learn-x")
	p := &PR{Credentials: &credential.Fake{Credential: credential.Credential{Token: fakeToken}}}
	if _, err := p.Publish(ctx, Change{Worktree: w, Title: "Apply", Body: "b"}); err == nil || !strings.Contains(err.Error(), "isn't a hosted repository") {
		t.Fatalf("publish err = %v", err)
	}
	if _, _, err := p.Committer(ctx, w); err == nil {
		t.Error("Committer answered for an origin that names no host")
	}
}

func TestPRVerdictWithCredentials(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	gh, log := filepath.Join(dir, "gh"), filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"$GH_TOKEN\" \"$*\" >> '" + log + "'\n" +
		"echo '{\"state\":\"MERGED\",\"mergedAt\":\"2026-03-02T10:00:00Z\",\"mergeCommit\":{\"oid\":\"abc\"},\"commits\":[{},{}]}'\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	creds := &credential.Fake{Credential: credential.Credential{Token: fakeToken}}
	p := &PR{GH: gh, Credentials: creds}
	pub := Published{Where: "https://github.com/you/dotfiles/pull/7"}
	v, when, err := p.Verdict(ctx, pub)
	if err != nil || v != Merged || when != "2026-03-02" {
		t.Fatalf("verdict = %s %s %v", v, when, err)
	}
	if r, ok := p.Range(ctx, pub); !ok || r.To != "abc" || r.Commits != 2 {
		t.Errorf("range = %+v, %v", r, ok)
	}
	if len(creds.Asked) != 2 || creds.Asked[0] != "github.com/you/dotfiles" {
		t.Errorf("token asked for %v, want the pull request's repo each time", creds.Asked)
	}
	calls, _ := os.ReadFile(log)
	for _, l := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if !strings.HasPrefix(l, fakeToken+"|pr view ") {
			t.Errorf("gh call = %s, want pr view with the token in the environment", l)
		}
	}
	// A place that names no repo has no token to ask for.
	if _, _, err := p.Verdict(ctx, Published{Where: "branch learn-x"}); err == nil {
		t.Error("verdict on a non-URL succeeded with credentials")
	}
}

func TestPullRequestSlug(t *testing.T) {
	for where, want := range map[string]string{
		"https://github.com/you/dotfiles/pull/12":  "github.com/you/dotfiles",
		"https://GitHub.com/you/dotfiles/pull/12/": "github.com/you/dotfiles",
		"https://github.com/you/dotfiles":          "",
		"https://github.com/you/dotfiles/issues/3": "",
		"branch learn-x":                           "",
	} {
		if got, _ := pullRequestSlug(where); got != want {
			t.Errorf("pullRequestSlug(%q) = %q, want %q", where, got, want)
		}
	}
}

// A worktree with an author commits as that author, unsigned, whatever the
// machine's identity and signing config; without one, nothing changes.
func TestWorktreeCommitsAsItsAuthor(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	// The owner signs every commit; a bot's commit must not carry that
	// signature, and here the signing program doesn't even exist.
	global := os.Getenv("GIT_CONFIG_GLOBAL")
	w, err := AddWorktree(ctx, live, filepath.Join(t.TempDir(), "wt"), "learn-x")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "CLAUDE.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CommitPaths(ctx, "add: mine", []string{"CLAUDE.md"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := run(t, w.Dir, "log", "-1", "--format=%an <%ae> / %cn <%ce>"); got != "you <you@example.com> / you <you@example.com>" {
		t.Errorf("without an author, commit is by %s", got)
	}

	run(t, live, "config", "--file", global, "commit.gpgsign", "true")
	run(t, live, "config", "--file", global, "gpg.program", "/nonexistent/gpg")
	w.AuthorName, w.AuthorEmail = "recall-you[bot]", "4242+recall-you[bot]@users.noreply.github.com"
	if err := os.WriteFile(filepath.Join(w.Dir, "CLAUDE.md"), []byte("the bot's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CommitPaths(ctx, "add: the bot's", []string{"CLAUDE.md"}, nil); err != nil {
		t.Fatal(err)
	}
	want := "recall-you[bot] <4242+recall-you[bot]@users.noreply.github.com>"
	if got := run(t, w.Dir, "log", "-1", "--format=%an <%ae> / %cn <%ce>"); got != want+" / "+want {
		t.Errorf("commit is by %s, want author and committer %s", got, want)
	}
	if sig := run(t, w.Dir, "log", "-1", "--format=%G?"); sig != "N" {
		t.Errorf("signature status = %q, want none", sig)
	}
}

func TestPRCommitter(t *testing.T) {
	ctx := context.Background()
	live := clone(t)
	w := committed(t, live, "learn-x")
	run(t, live, "remote", "set-url", "origin", "git@github.com:you/dotfiles.git")
	if name, email, err := (&PR{}).Committer(ctx, w); name != "" || email != "" || err != nil {
		t.Errorf("without credentials = %q %q %v, want the machine's identity kept", name, email, err)
	}
	p := &PR{Credentials: &credential.Fake{Credential: credential.Credential{Token: fakeToken, Name: "recall-you[bot]", Email: "4242+recall-you[bot]@users.noreply.github.com"}}}
	name, email, err := p.Committer(ctx, w)
	if err != nil || name != "recall-you[bot]" || !strings.HasPrefix(email, "4242+") {
		t.Errorf("committer = %q %q %v", name, email, err)
	}
}
