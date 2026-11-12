package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Change is one repo's committed edits, waiting for the owner's review.
type Change struct {
	Worktree *Worktree
	// Title and Body describe the change for the reviewer.
	Title, Body string
}

// Verdict is what became of a published change.
type Verdict string

const (
	Pending Verdict = "pending"
	Merged  Verdict = "merged"
	Closed  Verdict = "closed" // rejected: closed or deleted without merging
)

// Published is a change as recorded when it was published, enough to ask
// later what became of it.
type Published struct {
	Where, Repo, Branch, Base, Commit string
}

// Publisher hands a committed change to the owner for review and says
// where it landed, like a pull request URL, then reports the owner's
// verdict on it.
type Publisher interface {
	Publish(ctx context.Context, c Change) (string, error)
	// Verdict reports what became of a published change, and the day it
	// was decided (YYYY-MM-DD), when the publisher knows it.
	Verdict(ctx context.Context, p Published) (Verdict, string, error)
}

// publishers is the registry learn.review names.
var publishers = map[string]func() Publisher{
	"pr":     func() Publisher { return &PR{} },
	"branch": func() Publisher { return Branch{} },
}

// New returns the publisher registered under name.
func New(name string) (Publisher, error) {
	f, ok := publishers[name]
	if !ok {
		return nil, fmt.Errorf("review %q: want one of %s", name, strings.Join(Names(), ", "))
	}
	return f(), nil
}

// Names lists the registered publishers.
func Names() []string {
	names := make([]string, 0, len(publishers))
	for n := range publishers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// PR pushes the branch to origin and opens a pull request with gh.
type PR struct {
	// GH is the gh binary, default "gh".
	GH string
}

func (p *PR) Publish(ctx context.Context, c Change) (string, error) {
	w := c.Worktree
	if w.Remote == "" {
		return "", fmt.Errorf("%s has no origin remote to open a pull request on; set learn.review = \"branch\"", w.Repo)
	}
	if _, err := git(ctx, w.Dir, "", "push", "-q", "-u", w.Remote, w.Branch); err != nil {
		return "", err
	}
	gh := p.GH
	if gh == "" {
		gh = "gh"
	}
	cmd := exec.CommandContext(ctx, gh, "pr", "create", "--base", w.BaseBranch(), "--head", w.Branch,
		"--title", c.Title, "--body-file", "-")
	cmd.Dir = w.Dir
	cmd.Stdin = strings.NewReader(c.Body)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errOut.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("gh pr create in %s (branch %s is pushed): %s", w.Repo, w.Branch, detail)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

// Verdict asks gh for the pull request's state; squash and rebase merges
// count as merged, which a branch's history alone can't tell.
func (p *PR) Verdict(ctx context.Context, pub Published) (Verdict, string, error) {
	gh := p.GH
	if gh == "" {
		gh = "gh"
	}
	cmd := exec.CommandContext(ctx, gh, "pr", "view", pub.Where, "--json", "state,mergedAt,closedAt")
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errOut.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", "", fmt.Errorf("gh pr view %s: %s", pub.Where, detail)
	}
	var v struct {
		State    string `json:"state"`
		MergedAt string `json:"mergedAt"`
		ClosedAt string `json:"closedAt"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		return "", "", fmt.Errorf("gh pr view %s: %w", pub.Where, err)
	}
	switch v.State {
	case "OPEN":
		return Pending, "", nil
	case "MERGED":
		return Merged, day(v.MergedAt), nil
	case "CLOSED":
		return Closed, day(v.ClosedAt), nil
	}
	return "", "", fmt.Errorf("gh pr view %s: unknown state %q", pub.Where, v.State)
}

func day(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ""
}

// Branch leaves the committed branch in the repo for the owner to review
// and merge by hand; nothing leaves the machine.
type Branch struct{}

func (Branch) Publish(ctx context.Context, c Change) (string, error) {
	return "branch " + c.Worktree.Branch, nil
}

// Verdict reads the branch's fate from history: its commit reachable from
// the base is merged, the branch still there is pending, and the branch
// deleted without merging is closed. A squash or cherry-pick looks closed,
// so merge the branch itself.
func (Branch) Verdict(ctx context.Context, p Published) (Verdict, string, error) {
	if p.Commit == "" {
		return Pending, "", nil
	}
	bases := []string{p.Base}
	if _, local, ok := strings.Cut(p.Base, "/"); ok {
		bases = append(bases, local) // the owner may merge into the local branch
	}
	for _, b := range bases {
		if exists(ctx, p.Repo, b) {
			if _, err := git(ctx, p.Repo, "", "merge-base", "--is-ancestor", p.Commit, b); err == nil {
				return Merged, "", nil
			}
		}
	}
	if exists(ctx, p.Repo, "refs/heads/"+p.Branch) {
		return Pending, "", nil
	}
	return Closed, "", nil
}

// Fake records what it was asked to publish and answers verdicts from a
// map keyed by where; a missing key is pending.
type Fake struct {
	Changes  []Change
	Err      error
	Verdicts map[string]Verdict
	Decided  string
}

func (f *Fake) Verdict(ctx context.Context, p Published) (Verdict, string, error) {
	if v, ok := f.Verdicts[p.Where]; ok {
		return v, f.Decided, nil
	}
	return Pending, "", nil
}

func (f *Fake) Publish(ctx context.Context, c Change) (string, error) {
	f.Changes = append(f.Changes, c)
	if f.Err != nil {
		return "", f.Err
	}
	return "fake:" + c.Worktree.Branch, nil
}
