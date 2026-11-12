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
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
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

// Worktree is one repo's staged checkout on a new branch.
type Worktree struct {
	// Repo is the live checkout; Dir is the worktree edits go to.
	Repo, Dir string
	Branch    string
	// Remote is "origin" when the repo has one, else empty; Base is the ref
	// the branch starts from, like origin/main.
	Remote, Base string
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
	out, err := gitRaw(ctx, w.Dir, "", "status", "--porcelain=v1", "-z", "--untracked-files=all")
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

// Stage adds every change and returns the staged diff, for a check to read.
func (w *Worktree) Stage(ctx context.Context) (string, error) {
	if _, err := git(ctx, w.Dir, "", "add", "-A"); err != nil {
		return "", err
	}
	return gitRaw(ctx, w.Dir, "", "diff", "--cached")
}

// Commit commits what's staged with title and body as the message.
func (w *Worktree) Commit(ctx context.Context, title, body string) error {
	msg := title
	if strings.TrimSpace(body) != "" {
		msg += "\n\n" + strings.TrimRight(body, "\n")
	}
	_, err := git(ctx, w.Dir, msg+"\n", "commit", "-q", "-F", "-")
	return err
}

// Head is the commit the worktree's branch points at.
func (w *Worktree) Head(ctx context.Context) (string, error) {
	return git(ctx, w.Dir, "", "rev-parse", "HEAD")
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
