// Package review stages an edit for the owner's review: a fresh git
// worktree per repo, a commit on a new branch, and a Publisher that hands
// the branch over (a pull request, or the branch left in place). Nothing
// here writes a live file; every edit lands in a worktree first.
package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// git runs git in dir with stdin and returns its trimmed stdout. A failure
// carries git's stderr.
func git(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, stdin, args...)
	return strings.TrimSpace(out), err
}

// gitRaw is git with stdout as written, for output where leading
// whitespace means something.
func gitRaw(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	return gitEnv(ctx, dir, stdin, nil, args...)
}

// gitEnv is gitRaw with env added to the process's own environment: where
// a key repeats, env wins.
func gitEnv(ctx context.Context, dir, stdin string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errOut.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), detail)
	}
	return out.String(), nil
}

// configEnv returns env that hands git the given config pairs for one
// process, the way -c does but out of argv: GIT_CONFIG_COUNT and its
// numbered keys and values, after any the environment already carries.
// Later pairs win, and these outrank every config file.
func configEnv(pairs ...[2]string) []string {
	n := 0
	if v, err := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT")); err == nil && v > 0 {
		n = v
	}
	env := make([]string, 0, 2*len(pairs)+1)
	for _, kv := range pairs {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, kv[1]))
		n++
	}
	return append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", n))
}

// Resolve returns path with symlinks evaluated, for a path that need not
// exist yet: the nearest existing ancestor is resolved and the rest kept.
func Resolve(path string) string {
	path = filepath.Clean(path)
	var rest []string
	for p := path; ; p = filepath.Dir(p) {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return real
		}
		if filepath.Dir(p) == p {
			return path
		}
		rest = append(rest, filepath.Base(p))
	}
}

// RepoRoot returns the top of the git checkout that holds path, which need
// not exist yet. ok is false when no checkout holds it.
func RepoRoot(ctx context.Context, path string) (root string, ok bool) {
	dir := Resolve(path)
	for {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
	top, err := git(ctx, dir, "", "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return "", false
	}
	return Resolve(top), true
}

// IgnoredLive reports whether an ignore rule matches path in the checkout
// that holds it and the index doesn't already track it. A path outside any
// checkout is not ignored, and the path need not exist yet.
func IgnoredLive(ctx context.Context, path string) bool {
	root, ok := RepoRoot(ctx, path)
	if !ok {
		return false
	}
	rel, err := filepath.Rel(root, Resolve(path))
	if err != nil {
		return false
	}
	_, gerr := git(ctx, root, "", "check-ignore", "-q", "--", rel)
	return gerr == nil
}

// Worktree is one repo's staged checkout on a new branch.
type Worktree struct {
	// Repo is the live checkout; Dir is the worktree edits go to.
	Repo, Dir string
	Branch    string
	// Remote is "origin" when the repo has one, else empty; Base is the ref
	// the branch starts from, like origin/main.
	Remote, Base string
	// AuthorName and AuthorEmail, when both are set, author and commit
	// the worktree's commits in place of the machine's git identity: the
	// bot a publisher opens the pull request as, so the commits match it.
	AuthorName, AuthorEmail string
	// start is the commit the branch started from, for Committed.
	start string
}

// commitEnv is the environment a commit runs with: nothing, or the
// worktree's own author. A commit made as someone else is never signed
// with the machine owner's key, which would vouch for an author it isn't.
func (w *Worktree) commitEnv() []string {
	if w.AuthorName == "" || w.AuthorEmail == "" {
		return nil
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + w.AuthorName, "GIT_AUTHOR_EMAIL=" + w.AuthorEmail,
		"GIT_COMMITTER_NAME=" + w.AuthorName, "GIT_COMMITTER_EMAIL=" + w.AuthorEmail,
	}
	return append(env, configEnv([2]string{"commit.gpgsign", "false"})...)
}

// AddWorktree fetches root's origin, when it has one, and checks out a new
// branch at dir from origin's default branch, or from HEAD without an
// origin. A branch that already exists, here or on origin, is an error:
// it is an earlier apply's, waiting for review.
func AddWorktree(ctx context.Context, root, dir, branch string) (*Worktree, error) {
	w := &Worktree{Repo: root, Dir: dir, Branch: branch, Base: "HEAD"}
	remotes, err := git(ctx, root, "", "remote")
	if err != nil {
		return nil, err
	}
	for r := range strings.FieldsSeq(remotes) {
		if r == "origin" {
			w.Remote = r
		}
	}
	if w.Remote != "" {
		if _, err := git(ctx, root, "", "fetch", "-q", w.Remote); err != nil {
			return nil, err
		}
		w.Base = defaultBranch(ctx, root, w.Remote)
		if exists(ctx, root, "refs/remotes/"+w.Remote+"/"+branch) {
			return nil, fmt.Errorf("%s: branch %s already exists on %s; review or delete it first", root, branch, w.Remote)
		}
	}
	if exists(ctx, root, "refs/heads/"+branch) {
		return nil, fmt.Errorf("%s: branch %s already exists; review or delete it first", root, branch)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	if _, err := git(ctx, root, "", "worktree", "add", "-q", "--no-track", "-b", branch, dir, w.Base); err != nil {
		return nil, err
	}
	w.Dir = Resolve(dir)
	w.start, err = git(ctx, w.Dir, "", "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	return w, nil
}

// defaultBranch is the remote's HEAD, like origin/main, or origin/main or
// origin/master when the remote's HEAD isn't recorded locally.
func defaultBranch(ctx context.Context, root, remote string) string {
	if ref, err := git(ctx, root, "", "symbolic-ref", "-q", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil && ref != "" {
		return ref
	}
	for _, b := range []string{"main", "master"} {
		if exists(ctx, root, "refs/remotes/"+remote+"/"+b) {
			return remote + "/" + b
		}
	}
	return "HEAD"
}

func exists(ctx context.Context, root, ref string) bool {
	_, err := git(ctx, root, "", "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

// BaseBranch is Base without the remote, like main: the branch a pull
// request merges into.
func (w *Worktree) BaseBranch() string {
	if w.Remote != "" {
		return strings.TrimPrefix(w.Base, w.Remote+"/")
	}
	return w.Base
}

// Changed lists the paths, relative to the worktree, that differ from the
// branch's start: edited, added, or deleted, untracked files included.
func (w *Worktree) Changed(ctx context.Context) ([]string, error) {
	// A fresh worktree starts clean, so an ignored file present was
	// written by this apply; --ignored lists it with the rest.
	out, err := gitRaw(ctx, w.Dir, "", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored")
	if err != nil {
		return nil, err
	}
	var paths []string
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		paths = append(paths, e[3:])
		if e[0] == 'R' || e[0] == 'C' {
			i++ // a rename or copy is followed by its source path
		}
	}
	return paths, nil
}

// Ignored returns the subset of paths, relative to the worktree, that an
// ignore rule matches and the index doesn't already track.
func (w *Worktree) Ignored(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "git", "check-ignore", "-z", "--stdin")
	cmd.Dir = w.Dir
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00"))
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		err = nil // no path is ignored
	}
	if err != nil {
		detail := strings.TrimSpace(errOut.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("git check-ignore: %s", detail)
	}
	var ignored []string
	for _, p := range strings.Split(out.String(), "\x00") {
		if p != "" {
			ignored = append(ignored, p)
		}
	}
	return ignored, nil
}

// Stage stages exactly the given paths, relative to the worktree, and
// returns the staged diff, for a check to read. Each path is forced past
// any ignore rule, so a file the apply wrote never falls out of the change
// silently; Ignored says which paths that force mattered for.
func (w *Worktree) Stage(ctx context.Context, paths []string) (string, error) {
	if len(paths) > 0 {
		args := append([]string{"add", "-f", "--"}, paths...)
		if _, err := git(ctx, w.Dir, "", args...); err != nil {
			return "", err
		}
	}
	return gitRaw(ctx, w.Dir, "", "diff", "--cached")
}

// Unstage empties the index, so commits can be built path by path after
// Stage read the whole diff.
func (w *Worktree) Unstage(ctx context.Context) error {
	_, err := git(ctx, w.Dir, "", "reset", "-q")
	return err
}

// Committed lists the paths, relative to the worktree, that the branch's
// commits touch since its base, across every commit the apply made.
func (w *Worktree) Committed(ctx context.Context) ([]string, error) {
	base := w.start
	if base == "" {
		base = w.Base // a worktree rebuilt from its fields, not AddWorktree
	}
	out, err := gitRaw(ctx, w.Dir, "", "diff", "--name-only", "-z", base, "HEAD")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// Commit commits what's staged with title and body as the message.
func (w *Worktree) Commit(ctx context.Context, title, body string) error {
	msg := title
	if strings.TrimSpace(body) != "" {
		msg += "\n\n" + strings.TrimRight(body, "\n")
	}
	_, err := gitEnv(ctx, w.Dir, msg+"\n", w.commitEnv(), "commit", "-q", "-F", "-")
	return err
}

// CommitPaths stages the changes under rels plus rest and commits them with
// msg. It returns the new commit, or "" when nothing was left to stage,
// which happens when an earlier commit already carried the same file. Each
// path is forced past any ignore rule, like Stage: callers pass only paths
// a Stage already vetted, so a file the apply wrote never falls out of a
// commit silently.
func (w *Worktree) CommitPaths(ctx context.Context, msg string, rels, rest []string) (string, error) {
	for _, rel := range append(append([]string{}, rels...), rest...) {
		if _, err := git(ctx, w.Dir, "", "add", "-f", "-A", "--", rel); err != nil {
			return "", err
		}
	}
	if _, err := git(ctx, w.Dir, "", "diff", "--cached", "--quiet"); err == nil {
		return "", nil // nothing staged
	}
	if _, err := gitEnv(ctx, w.Dir, msg+"\n", w.commitEnv(), "commit", "-q", "-F", "-"); err != nil {
		return "", err
	}
	return git(ctx, w.Dir, "", "rev-parse", "HEAD")
}

// Head is the commit the worktree's branch points at.
func (w *Worktree) Head(ctx context.Context) (string, error) {
	return git(ctx, w.Dir, "", "rev-parse", "HEAD")
}

// Restore puts one staged file back the way it was when the stage was
// made, undoing whatever was written to it: its original content, or gone
// if the stage created it.
func (w *Worktree) Restore(ctx context.Context, rel string) error {
	at := filepath.Join(w.Dir, rel)
	if _, err := git(ctx, w.Dir, "", "cat-file", "-e", "HEAD:"+filepath.ToSlash(rel)); err != nil {
		// Not in the branch's start: the stage created it.
		if err := os.Remove(at); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	_, err := git(ctx, w.Dir, "", "checkout", "-q", "HEAD", "--", rel)
	return err
}

// Remove deletes the worktree, and the branch too unless keepBranch.
func (w *Worktree) Remove(ctx context.Context, keepBranch bool) error {
	_, err := git(ctx, w.Repo, "", "worktree", "remove", "--force", w.Dir)
	if !keepBranch {
		_, berr := git(ctx, w.Repo, "", "branch", "-q", "-D", w.Branch)
		err = errors.Join(err, berr)
	}
	return err
}
