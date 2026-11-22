package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/micahhausler/recall/internal/credential"
)

// Change is one repo's committed edits, waiting for the owner's review.
type Change struct {
	Worktree *Worktree
	// Title and Body describe the change for the reviewer.
	Title, Body string
	// Proposals are the titles of the proposals the change applies.
	Proposals []string
}

// Verdict is what became of a published change.
type Verdict string

const (
	Pending Verdict = "pending"
	Merged  Verdict = "merged"
	Closed  Verdict = "closed" // rejected: closed or deleted without merging
)

// Published is a change as recorded when it was published, enough to ask
// later what became of it. Commit is the published proposal's own commit:
// the apply stages one commit per proposal, so the owner can drop one
// before merging and each proposal gets its own verdict.
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
	// Credentials, when set, are who the publisher acts as, in place of
	// the machine's own login: the push goes to origin's HTTPS URL with
	// their token, since a token can't authenticate an SSH remote, and gh
	// runs with the same token. A repo they don't cover fails the publish;
	// nothing falls back to the machine's login. When the credential names
	// its owner, the pull request asks them for review, so a change a bot
	// opened reaches the owner as a review request.
	Credentials credential.Source
	// Assignees are the logins assigned to each pull request once it is
	// open (review.assignees), with or without credentials. Empty assigns
	// no one.
	Assignees []string
	// Log gets a warning that doesn't fail a publish; nil is os.Stderr.
	Log io.Writer
	// base overrides the scheme and host a credentialed push goes to, so a
	// test can stand in for the forge.
	base string
}

func (p *PR) gh() string {
	if p.GH == "" {
		return "gh"
	}
	return p.GH
}

// originSlug is the worktree's origin as host/owner/name, or "" when the
// origin isn't a hosted URL.
func originSlug(ctx context.Context, w *Worktree) string {
	if w.Remote == "" {
		return ""
	}
	url, err := git(ctx, w.Dir, "", "remote", "get-url", w.Remote)
	if err != nil {
		return ""
	}
	return RemoteSlug(url)
}

// Committer is the credentials' commit identity for the worktree's origin,
// or nothing without credentials.
func (p *PR) Committer(ctx context.Context, w *Worktree) (name, email string, err error) {
	if p.Credentials == nil {
		return "", "", nil
	}
	slug := originSlug(ctx, w)
	if slug == "" {
		return "", "", fmt.Errorf("%s has no hosted origin to open a pull request on", w.Repo)
	}
	cred, err := p.Credentials.For(ctx, slug)
	if err != nil {
		return "", "", err
	}
	return cred.Name, cred.Email, nil
}

func (p *PR) Publish(ctx context.Context, c Change) (string, error) {
	w := c.Worktree
	if w.Remote == "" {
		return "", fmt.Errorf("%s has no origin remote to open a pull request on; set learn.review = \"branch\"", w.Repo)
	}
	slug := originSlug(ctx, w)
	// Without credentials the push goes to origin as the machine's own
	// login. With them it goes to origin's HTTPS URL, named outright so the
	// token is used whatever protocol the checkout was cloned over.
	target := w.Remote
	push := []string{"push", "-q", "-u", target, w.Branch}
	var gitEnvs, ghEnvs []string
	reviewer := ""
	if p.Credentials != nil {
		a, err := auth(ctx, p.Credentials, slug, p.base)
		if err != nil {
			return "", fmt.Errorf("%s: %w", w.Repo, err)
		}
		target, gitEnvs, ghEnvs, reviewer = a.url, a.git, a.gh, a.owner
		push = []string{"push", "-q", target, "refs/heads/" + w.Branch + ":refs/heads/" + w.Branch}
	}
	if _, err := gitEnv(ctx, w.Dir, "", gitEnvs, push...); err != nil {
		return "", err
	}
	args := []string{"pr", "create", "--base", w.BaseBranch(), "--head", w.Branch, "--title", c.Title, "--body-file", "-"}
	// Name origin as the repo the pull request opens on. In a fork, gh
	// would otherwise aim it at the repo the fork came from, and a change
	// learned from private sessions would land in front of other people.
	if slug != "" {
		args = append(args, "--repo", strings.TrimPrefix(slug, "github.com/"))
	}
	cmd := exec.CommandContext(ctx, p.gh(), args...)
	cmd.Dir = w.Dir
	if len(ghEnvs) > 0 {
		cmd.Env = append(os.Environ(), ghEnvs...)
	}
	cmd.Stdin = strings.NewReader(c.Body)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errOut.String())
		if detail == "" {
			detail = err.Error()
		}
		// Take back the branch this call pushed above, so the next apply
		// isn't refused over a branch no pull request tracks. A branch that
		// existed on origin before the apply never gets here: AddWorktree
		// already refused it.
		cleanup := fmt.Sprintf("the pushed branch %s was deleted from %s", w.Branch, w.Remote)
		if _, derr := gitEnv(ctx, w.Dir, "", gitEnvs, "push", "-q", target, "--delete", w.Branch); derr != nil {
			cleanup = fmt.Sprintf("deleting the pushed branch %s from %s also failed: %v", w.Branch, w.Remote, derr)
		}
		return "", fmt.Errorf("gh pr create in %s: %s (%s)", w.Repo, detail, cleanup)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	where := strings.TrimSpace(lines[len(lines)-1])
	log := p.Log
	if log == nil {
		log = os.Stderr
	}
	if reviewer != "" {
		// The pull request is open either way; a review request that
		// didn't take costs the owner a notification, not the change.
		if err := p.requestReview(ctx, where, reviewer, ghEnvs); err != nil {
			fmt.Fprintf(log, "recall: warning: %s is open, but asking %s to review it failed: %v\n", where, reviewer, err)
		}
	}
	if len(p.Assignees) > 0 {
		// The same goes for an assignment that didn't take.
		if err := p.assign(ctx, where, p.Assignees, ghEnvs); err != nil {
			fmt.Fprintf(log, "recall: warning: %s is open, but assigning it to %s failed: %v\n", where, strings.Join(p.Assignees, ", "), err)
		}
	}
	return where, nil
}

// assign assigns the pull request at where to logins. Like the review
// request, it is its own call after the pull request exists. The forge
// answers a login it can't assign with success and leaves it out, so the
// answer is read: a login missing from it is an error that names it.
func (p *PR) assign(ctx context.Context, where string, logins, env []string) error {
	slug, number, ok := pullRequestRef(where)
	if !ok {
		return fmt.Errorf("%q isn't a pull request URL", where)
	}
	_, owner, name, _ := splitSlug(slug)
	args := []string{"api", "--method", "POST", "--jq", ".assignees[].login",
		"repos/" + owner + "/" + name + "/issues/" + number + "/assignees"}
	for _, login := range logins {
		args = append(args, "-f", "assignees[]="+login)
	}
	cmd := exec.CommandContext(ctx, p.gh(), args...)
	cmd.Env = append(os.Environ(), env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(errOut.String()); detail != "" {
			return errors.New(detail)
		}
		return err
	}
	assigned := strings.Fields(out.String())
	var missing []string
	for _, login := range logins {
		if !slices.ContainsFunc(assigned, func(got string) bool { return strings.EqualFold(got, login) }) {
			missing = append(missing, login)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the forge accepted the request but didn't assign %s; only someone with access to the repository can be assigned", strings.Join(missing, ", "))
	}
	return nil
}

// requestReview asks login to review the pull request at where. It is its
// own call, after the pull request exists, so a refusal can't undo the
// publish the way a failed gh pr create --reviewer would.
func (p *PR) requestReview(ctx context.Context, where, login string, env []string) error {
	slug, number, ok := pullRequestRef(where)
	if !ok {
		return fmt.Errorf("%q isn't a pull request URL", where)
	}
	_, owner, name, _ := splitSlug(slug)
	cmd := exec.CommandContext(ctx, p.gh(), "api", "--method", "POST", "--silent",
		"repos/"+owner+"/"+name+"/pulls/"+number+"/requested_reviewers", "-f", "reviewers[]="+login)
	cmd.Env = append(os.Environ(), env...)
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(errOut.String()); detail != "" {
			return errors.New(detail)
		}
		return err
	}
	return nil
}

// view runs gh pr view on a published pull request, as the credentials
// when there are any.
func (p *PR) view(ctx context.Context, where, fields string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.gh(), "pr", "view", where, "--json", fields)
	if p.Credentials != nil {
		slug, ok := pullRequestSlug(where)
		if !ok {
			return nil, fmt.Errorf("gh pr view %s: not a pull request URL", where)
		}
		a, err := auth(ctx, p.Credentials, slug, p.base)
		if err != nil {
			return nil, fmt.Errorf("gh pr view %s: %w", where, err)
		}
		cmd.Env = append(os.Environ(), a.gh...)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errOut.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("gh pr view %s: %s", where, detail)
	}
	return out.Bytes(), nil
}

// Range asks gh for a merged pull request's merge commit and commit count,
// so a hand-edited proposal is judged by what that merge alone added.
func (p *PR) Range(ctx context.Context, pub Published) (MergeRange, bool) {
	out, err := p.view(ctx, pub.Where, "mergeCommit,commits")
	if err != nil {
		return MergeRange{}, false
	}
	var v struct {
		MergeCommit *struct {
			Oid string `json:"oid"`
		} `json:"mergeCommit"`
		Commits []json.RawMessage `json:"commits"`
	}
	if json.Unmarshal(out, &v) != nil || v.MergeCommit == nil || v.MergeCommit.Oid == "" {
		return MergeRange{}, false
	}
	return MergeRange{To: v.MergeCommit.Oid, Commits: len(v.Commits)}, true
}

// Verdict asks gh for the pull request's state; squash and rebase merges
// count as merged, which a branch's history alone can't tell.
func (p *PR) Verdict(ctx context.Context, pub Published) (Verdict, string, error) {
	out, err := p.view(ctx, pub.Where, "state,mergedAt,closedAt")
	if err != nil {
		return "", "", err
	}
	var v struct {
		State    string `json:"state"`
		MergedAt string `json:"mergedAt"`
		ClosedAt string `json:"closedAt"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
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

// MergeRange names what one merged change added to its base: the commit
// the merge made (To) and how many commits the change carried. The zero
// range means unknown.
type MergeRange struct {
	To      string
	Commits int
}

// from is the base just before the merge: a merge commit's first parent,
// or, for a rebase or squash merge, the commit the change's commits sit
// on. A squash leaves one commit where the change had several, so that
// reaches back past it, taking in a few commits merged before it.
func (r MergeRange) from(ctx context.Context, repo string) (string, bool) {
	parents, err := git(ctx, repo, "", "rev-list", "--parents", "-n", "1", r.To)
	if err != nil {
		return "", false
	}
	if len(strings.Fields(parents)) > 2 {
		return r.To + "^1", true
	}
	n := max(r.Commits, 1)
	from := fmt.Sprintf("%s~%d", r.To, n)
	if !exists(ctx, repo, from) {
		return "", false
	}
	return from, true
}

// Ranger is a publisher that can say exactly what a merged change added.
type Ranger interface {
	Range(ctx context.Context, p Published) (MergeRange, bool)
}

// RuleLanded reports whether a review's proposal is in the base its change
// merged into. The owner can drop one proposal's commit, or take its text
// out, before merging, so a merged change doesn't prove every proposal in
// it landed. The proposal's own commit being an ancestor of the base is the
// strongest signal and wins whatever the wording became; a squash or rebase
// merge loses that commit, so the fallback is the ledger's record of the
// rule text that landed, matched as an exact whole bullet line so a short
// rule can't match text that was already in the base. Last, a proposal the
// owner reworded by hand before merging landed too (editedLanded). ok is
// false when there is nothing to check against: no repo, no rule text, or a
// remote base that can't be read.
func RuleLanded(ctx context.Context, p Published, rule string, scope MergeRange) (landed, ok bool) {
	// Rules are top-level bullets, one line each; sub-bullets under a rule
	// are detail, so the first line identifies it.
	rule = strings.TrimSpace(strings.Split(rule, "\n")[0])
	if p.Repo == "" || p.Base == "" || rule == "" {
		return false, false
	}
	if st, err := os.Stat(p.Repo); err != nil || !st.IsDir() {
		return false, false
	}
	bases := []string{p.Base}
	if remote, local, isRemote := strings.Cut(p.Base, "/"); isRemote {
		// The merge may have happened on the remote, as a pull request does;
		// read it fresh. When the remote can't be read, judge by local refs
		// alone rather than by a stale remote-tracking ref.
		if _, err := git(ctx, p.Repo, "", "fetch", "-q", remote); err != nil {
			bases = bases[:0]
		}
		bases = append(bases, local)
	}
	checked := false
	for _, b := range bases {
		if b == "" || !exists(ctx, p.Repo, b) {
			continue
		}
		checked = true
		if p.Commit != "" {
			if _, err := git(ctx, p.Repo, "", "merge-base", "--is-ancestor", p.Commit, b); err == nil {
				return true, true
			}
		}
		bullet := "^[-*][[:space:]]+" + regexp.QuoteMeta(rule) + "$"
		if _, err := git(ctx, p.Repo, "", "grep", "-q", "-E", "-e", bullet, b); err == nil {
			return true, true
		}
	}
	if checked && editedLanded(ctx, p, bases, rule, scope) {
		return true, true
	}
	return false, checked
}

// editedMinContainment is how much of the proposed rule's wording, as the
// share of its words found in a new bullet, marks that bullet as the
// owner's rewording of it. Measured against the proposal alone, so words
// the owner adds don't lower it; a hand edit keeps most of the proposal
// (0.85 and up in practice), while unrelated rules share under 0.4.
const editedMinContainment = 0.6

// editedMinWords keeps a short proposal from matching any bullet that
// happens to contain its few words.
const editedMinWords = 4

// editedLanded reports whether the owner merged a proposal after rewording
// it: a top-level bullet the merge added that holds most of the proposed
// rule's words. The bullet has to be new, so a rejected reword can't match
// the rule it would have replaced, which reads much like it.
//
// What counts as new is as narrow as the publisher can say. With a merge
// range (a pull request's merge commit), it is a bullet the merge itself
// added, in any file it changed. Without one, it is a bullet in a file the
// proposal's commit changed that wasn't there at that commit's parent; that
// window also takes in anything else merged into the file since, and needs
// the proposal's commit, which git prunes some weeks after its branch is
// deleted.
func editedLanded(ctx context.Context, p Published, bases []string, rule string, scope MergeRange) bool {
	want := tokens(rule)
	if len(want) < editedMinWords {
		return false
	}
	type window struct{ files, from, to string }
	var windows []window
	from, scoped := "", false
	if scope.To != "" && exists(ctx, p.Repo, scope.To) {
		from, scoped = scope.from(ctx, p.Repo)
	}
	if scoped {
		windows = append(windows, window{from + ".." + scope.To, from, scope.To})
	} else if p.Commit != "" && exists(ctx, p.Repo, p.Commit) {
		for _, b := range bases {
			if b != "" && exists(ctx, p.Repo, b) {
				windows = append(windows, window{p.Commit + "^.." + p.Commit, p.Commit + "^", b})
			}
		}
	}
	for _, w := range windows {
		files, err := gitRaw(ctx, p.Repo, "", "diff", "-z", "--name-only", w.files)
		if err != nil {
			continue
		}
		for _, f := range strings.Split(files, "\x00") {
			if f == "" {
				continue
			}
			after, err := gitRaw(ctx, p.Repo, "", "show", w.to+":"+f)
			if err != nil {
				continue
			}
			before, _ := gitRaw(ctx, p.Repo, "", "show", w.from+":"+f)
			old := map[string]bool{}
			for _, b := range bullets(before) {
				old[b] = true
			}
			for _, b := range bullets(after) {
				if !old[b] && containment(want, tokens(b)) >= editedMinContainment {
					return true
				}
			}
		}
	}
	return false
}

var bulletLine = regexp.MustCompile(`^[-*]\s+(.*\S)\s*$`)

// bullets lists a Markdown file's top-level bullet texts.
func bullets(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if m := bulletLine.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// containment is the share of want's tokens that have holds.
func containment(want, have map[string]bool) float64 {
	if len(want) == 0 {
		return 0
	}
	n := 0
	for t := range want {
		if have[t] {
			n++
		}
	}
	return float64(n) / float64(len(want))
}

// stopwords, tokens, and stem copy internal/learn/similar.go, which this
// package can't import (learn imports review); keep the two in step.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true,
	"but": true, "by": true, "for": true, "from": true, "has": true, "have": true, "in": true,
	"into": true, "is": true, "it": true, "its": true, "of": true, "on": true, "or": true,
	"so": true, "than": true, "that": true, "the": true, "their": true, "then": true,
	"there": true, "these": true, "this": true, "to": true, "was": true, "were": true,
	"when": true, "which": true, "who": true, "will": true, "with": true,
}

func tokens(text string) map[string]bool {
	set := map[string]bool{}
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, w := range words {
		if stopwords[w] {
			continue
		}
		if w = stem(w); w != "" {
			set[w] = true
		}
	}
	return set
}

func stem(w string) string {
	if len(w) <= 4 {
		return w
	}
	for _, suffix := range []string{"ing", "ed", "es", "s"} {
		if rest, ok := strings.CutSuffix(w, suffix); ok && len(rest) >= 3 {
			return rest
		}
	}
	return w
}

// Fake records what it was asked to publish and answers verdicts from a
// map keyed by where; a missing key is pending.
type Fake struct {
	Changes  []Change
	Err      error
	Verdicts map[string]Verdict
	Decided  string
	// Asked counts Verdict calls.
	Asked int
	// Name and Email, when set, are the Committer the fake answers with.
	Name, Email string
}

func (f *Fake) Committer(ctx context.Context, w *Worktree) (string, string, error) {
	return f.Name, f.Email, nil
}

func (f *Fake) Verdict(ctx context.Context, p Published) (Verdict, string, error) {
	f.Asked++
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
