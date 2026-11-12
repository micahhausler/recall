package learn

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/review"
)

//go:embed prompts/apply.md
var applyPrompt string

// Proposal is one block of a reduce's proposals.md.
type Proposal struct {
	Kind, Title string
	// Target and Name are the block's "- target:" and "- name:" values;
	// Rule is its "- rule:" text, with any lines under it.
	Target, Name, Rule string
	// Text is the whole block, heading included.
	Text string
}

var fieldRE = regexp.MustCompile(`^- ([a-z]+):\s?(.*)$`)

// ruleHash keys a proposal's rule text, ignoring how it wraps.
func ruleHash(rule string) string {
	if strings.TrimSpace(rule) == "" {
		return ""
	}
	return textHash(strings.Join(strings.Fields(rule), " "))
}

// ParseProposals returns the proposal blocks in a reduce's output, in
// order. Blocks under a section other than "## Proposals" aren't
// proposals; the notes the binary appends live there.
func ParseProposals(md string) []Proposal {
	var out []Proposal
	var cur *Proposal
	var lines, rule []string
	field := ""
	inProposals := true
	flush := func() {
		if cur != nil {
			cur.Text = strings.TrimRight(strings.Join(lines, "\n"), "\n")
			cur.Rule = strings.TrimSpace(strings.Join(rule, "\n"))
			out = append(out, *cur)
		}
		cur, lines, rule, field = nil, nil, nil, ""
	}
	for _, line := range strings.Split(md, "\n") {
		if rest, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			inProposals = strings.EqualFold(strings.TrimSpace(rest), "proposals")
			continue
		}
		if rest, ok := strings.CutPrefix(line, "### "); ok {
			flush()
			if inProposals {
				kind, title, _ := strings.Cut(rest, ":")
				cur = &Proposal{Kind: strings.ToLower(strings.TrimSpace(kind)), Title: strings.TrimSpace(title)}
				lines = []string{line}
			}
			continue
		}
		if cur == nil {
			continue
		}
		lines = append(lines, line)
		if m := fieldRE.FindStringSubmatch(line); m != nil {
			field = m[1]
			v := strings.TrimSpace(m[2])
			switch {
			case field == "target" && cur.Target == "":
				cur.Target = strings.Trim(v, "`")
			case field == "name" && cur.Name == "":
				cur.Name = strings.TrimPrefix(strings.Trim(v, "`"), "/")
			case field == "rule" && rule == nil:
				rule = []string{v}
			}
			continue
		}
		if field == "rule" && strings.TrimSpace(line) != "" {
			rule = append(rule, strings.TrimSpace(line)) // the rule's own sub-bullets
		}
	}
	flush()
	return out
}

// Applier stages a reduce's proposals as edits in fresh worktrees, one per
// repo they touch, and hands each changed repo to a review publisher.
// Nothing it does writes a live file.
type Applier struct {
	Editor    model.Editor
	Model     string
	Publisher review.Publisher
	// Targets are learn.targets; Skills and Bin are where a new skill and a
	// graduated script go. All are live, absolute paths.
	Targets     []string
	Skills, Bin string
	// Public lists live paths shared publicly (keep.public).
	Public []string
	// Taken are the skill and command names that already exist.
	Taken []string
	Home  string
	// Check, when set, is a shell command run in each changed worktree with
	// the staged diff on stdin; a nonzero exit holds that repo's change back.
	Check string
	// OwnerInstructions are extensions.learn-apply's text.
	OwnerInstructions string
	// Reviews are the ledger's earlier proposals and their verdicts: one
	// still pending, or the same text already rejected, is held back.
	Reviews []Review
	Clock   clock.Clock
	Out     io.Writer
}

// Route is one proposal's place in a plan.
type Route struct {
	Proposal
	// Edit are the live files the proposal changes; Script is a graduate's
	// new script.
	Edit   []string
	Script string
	// Skip, when set, is why the binary holds the proposal back from the
	// model.
	Skip string
}

// Plan is what an apply will do, before any worktree exists.
type Plan struct {
	// Dir is the reduce run's directory, holding proposals.md.
	Dir          string
	Date, Branch string
	Routes       []Route
	// Repos are the live checkouts the routes touch, in first-use order,
	// and Worktrees where each one's worktree goes.
	Repos     []string
	Worktrees []string
}

// Ready reports how many routes go to the model.
func (p *Plan) Ready() int {
	n := 0
	for _, r := range p.Routes {
		if r.Skip == "" {
			n++
		}
	}
	return n
}

// Outcome is what happened to one proposal.
type Outcome struct {
	Applied bool     `json:"applied"`
	Kind    string   `json:"kind"`
	Title   string   `json:"title"`
	Detail  string   `json:"detail,omitempty"`
	Paths   []string `json:"paths,omitempty"`
}

// Landed is one repo's change and where it went for review.
type Landed struct {
	Repo      string   `json:"repo"`
	Branch    string   `json:"branch"`
	Base      string   `json:"base"`
	Commit    string   `json:"commit,omitempty"`
	Where     string   `json:"where,omitempty"`
	Proposals []string `json:"proposals"`
	Error     string   `json:"error,omitempty"`
}

// ApplyResult is a finished apply, saved as apply.json beside proposals.md.
type ApplyResult struct {
	Date     string      `json:"date"`
	Changes  []Landed    `json:"changes"`
	Outcomes []Outcome   `json:"outcomes"`
	Usage    model.Usage `json:"-"`
}

// ErrApplied is a run directory an earlier apply already finished.
var ErrApplied = errors.New("already applied")

var (
	datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

func (a *Applier) norm(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "`")
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(a.Home, strings.TrimPrefix(p, "~"))
	}
	return review.Resolve(p)
}

// Plan reads dir's proposals.md and routes each proposal to the live files
// it changes and the repos that hold them. It creates nothing.
func (a *Applier) Plan(ctx context.Context, dir string) (*Plan, error) {
	if _, err := os.Stat(filepath.Join(dir, "apply.json")); err == nil {
		return nil, fmt.Errorf("%s: %w; delete its apply.json to apply it again", dir, ErrApplied)
	}
	md, err := os.ReadFile(filepath.Join(dir, "proposals.md"))
	if err != nil {
		return nil, err
	}
	p := &Plan{Dir: dir, Date: filepath.Base(dir)}
	if !datePattern.MatchString(p.Date) {
		p.Date = a.Clock.Now().Format("2006-01-02")
	}
	p.Branch = "learn-" + p.Date

	targets := map[string]bool{}
	for _, t := range a.Targets {
		targets[a.norm(t)] = true
	}
	taken := map[string]bool{}
	for _, t := range a.Taken {
		taken[strings.ToLower(t)] = true
	}
	roots := map[string]string{} // live path -> repo root, "" for none
	rootOf := func(path string) string {
		if r, ok := roots[path]; ok {
			return r
		}
		r, _ := review.RepoRoot(ctx, path)
		roots[path] = r
		return r
	}
	use := func(root string) {
		for _, r := range p.Repos {
			if r == root {
				return
			}
		}
		p.Repos = append(p.Repos, root)
	}

	for _, prop := range ParseProposals(string(md)) {
		r := Route{Proposal: prop, Skip: a.reviewed(prop)}
		if r.Skip == "" {
			a.route(&r, targets, taken, rootOf)
		}
		p.Routes = append(p.Routes, r)
		if r.Skip == "" {
			for _, path := range append(append([]string{}, r.Edit...), r.Script) {
				if path != "" {
					use(rootOf(path))
				}
			}
		}
	}
	return p, a.placeWorktrees(p)
}

// reviewed says why prop is held back for an earlier review, if it is: the
// same proposal still waits for the owner, or the same text was rejected.
func (a *Applier) reviewed(prop Proposal) string {
	h := ruleHash(prop.Rule)
	for _, rv := range a.Reviews {
		switch rv.Verdict {
		case string(review.Pending):
			if (rv.Kind == prop.Kind && rv.Title == prop.Title) || (h != "" && rv.RuleHash == h) {
				return "already waiting for review in " + rv.Place()
			}
		case string(review.Closed):
			if h != "" && rv.RuleHash == h {
				return fmt.Sprintf("the same text was rejected in %s on %s", rv.Place(), rv.Decided)
			}
		}
	}
	return ""
}

// route sets the live files r changes, or why it can't be applied.
func (a *Applier) route(r *Route, targets, taken map[string]bool, rootOf func(string) string) {
	prop := r.Proposal
	switch prop.Kind {
	case "add", "reword", "retire":
		t := a.norm(prop.Target)
		if prop.Target == "" || !targets[t] {
			r.Skip = fmt.Sprintf("target %q isn't in learn.targets", prop.Target)
			break
		}
		r.Edit = []string{t}
	case "skill":
		switch {
		case prop.Name != "" && !namePattern.MatchString(prop.Name):
			r.Skip = fmt.Sprintf("skill name %q isn't lowercase-with-hyphens", prop.Name)
		case prop.Name != "" && taken[strings.ToLower(prop.Name)]:
			r.Skip = fmt.Sprintf("/%s is already a skill or command", prop.Name)
		case prop.Name != "":
			r.Edit = []string{review.Resolve(filepath.Join(a.Skills, prop.Name, "SKILL.md"))}
		case prop.Target != "":
			r.Edit = []string{a.norm(prop.Target)}
		default:
			r.Skip = "a skill proposal names neither a new skill nor the skill it changes"
		}
	case "graduate":
		name := filepath.Base(strings.Trim(strings.TrimSpace(prop.Target), "`"))
		if prop.Target == "" || !namePattern.MatchString(strings.TrimSuffix(name, filepath.Ext(name))) {
			r.Skip = fmt.Sprintf("graduate target %q doesn't name a script", prop.Target)
			break
		}
		r.Script = review.Resolve(filepath.Join(a.Bin, name))
		// The prose rule it retires is in one of the targets.
		for _, t := range a.Targets {
			if t := a.norm(t); rootOf(t) != "" {
				r.Edit = append(r.Edit, t)
			}
		}
	default:
		r.Skip = fmt.Sprintf("unknown kind %q", prop.Kind)
	}
	if r.Skip == "" {
		need := r.Edit
		if r.Script != "" {
			need = []string{r.Script}
		}
		for _, path := range need {
			if rootOf(path) == "" {
				r.Skip = fmt.Sprintf("%s isn't in a git checkout, so there's nothing to review it in", path)
				break
			}
		}
	}
}

// placeWorktrees names a worktree directory per repo under the run.
func (a *Applier) placeWorktrees(p *Plan) error {
	seen := map[string]bool{}
	for _, repo := range p.Repos {
		base := filepath.Base(repo)
		if base == "" || base == "/" || base == "." {
			base = "repo"
		}
		name := base
		for i := 2; seen[name]; i++ {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		seen[name] = true
		p.Worktrees = append(p.Worktrees, filepath.Join(p.Dir, "worktrees", name))
	}
	return nil
}

// mapping translates live paths into worktree paths.
type mapping struct {
	wts    []*review.Worktree
	public map[*review.Worktree]bool
}

// at returns where live is edited, and in which worktree.
func (m mapping) at(live string) (string, *review.Worktree) {
	var best *review.Worktree
	for _, w := range m.wts {
		if within(live, w.Repo) && (best == nil || len(w.Repo) > len(best.Repo)) {
			best = w
		}
	}
	if best == nil {
		return "", nil
	}
	rel, _ := filepath.Rel(best.Repo, live)
	return filepath.Join(best.Dir, rel), best
}

func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

func (a *Applier) mapping(wts []*review.Worktree) mapping {
	m := mapping{wts: wts, public: map[*review.Worktree]bool{}}
	for _, w := range wts {
		for _, pub := range a.Public {
			if within(w.Repo, review.Resolve(pub)) {
				m.public[w] = true
			}
		}
	}
	return m
}

// BuildInput renders the apply model's input for plan, with each repo's
// worktree at wts (in plan.Repos order).
func (a *Applier) BuildInput(plan *Plan, wts []*review.Worktree) string {
	m := a.mapping(wts)
	var b strings.Builder
	b.WriteString(applyPrompt)

	b.WriteString("\n\n# WORKTREES\n\n")
	for _, w := range wts {
		vis := "private"
		if m.public[w] {
			vis = "public"
		}
		fmt.Fprintf(&b, "- %s stands in for %s (%s)\n", w.Dir, w.Repo, vis)
	}

	b.WriteString("\n# TARGETS\n\n")
	listed := false
	for _, t := range a.Targets {
		if at, _ := m.at(a.norm(t)); at != "" {
			fmt.Fprintf(&b, "- %s: edit at %s\n", t, at)
			listed = true
		}
	}
	if !listed {
		b.WriteString("(none in a worktree)\n")
	}

	if strings.TrimSpace(a.OwnerInstructions) != "" {
		b.WriteString("\n# OWNER INSTRUCTIONS\n\n")
		b.WriteString(strings.TrimRight(a.OwnerInstructions, "\n") + "\n")
	}

	b.WriteString("\n# PROPOSALS\n")
	for _, r := range plan.Routes {
		if r.Skip != "" {
			continue
		}
		b.WriteString("\n" + r.Text + "\n")
		if r.Script != "" {
			at, _ := m.at(r.Script)
			fmt.Fprintf(&b, "- script: %s\n", at)
			continue
		}
		for _, e := range r.Edit {
			at, _ := m.at(e)
			fmt.Fprintf(&b, "- edit: %s\n", at)
		}
	}
	return b.String()
}

// PlannedWorktrees are the worktrees plan would create, for a dry run's
// input; nothing exists at their paths yet.
func PlannedWorktrees(plan *Plan) []*review.Worktree {
	wts := make([]*review.Worktree, len(plan.Repos))
	for i, repo := range plan.Repos {
		wts[i] = &review.Worktree{Repo: repo, Dir: plan.Worktrees[i], Branch: plan.Branch}
	}
	return wts
}

var outcomeLine = regexp.MustCompile(`^(Applied|Skipped)\[([a-z]+)\]:\s*(.+)$`)

// ParseOutcomes reads the apply model's summary lines. Anything else in
// its output is ignored.
func ParseOutcomes(out string) []Outcome {
	var outcomes []Outcome
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		mt := outcomeLine.FindStringSubmatch(line)
		if mt == nil {
			continue
		}
		o := Outcome{Applied: mt[1] == "Applied", Kind: mt[2], Title: mt[3]}
		if o.Applied {
			// Paths never hold " -- "; a title might.
			if i := strings.LastIndex(mt[3], " -- "); i >= 0 {
				o.Title = mt[3][:i]
				rest := mt[3][i+4:]
				// A note after the paths, despite the prompt, isn't a path.
				if j := strings.Index(rest, " ("); j >= 0 {
					o.Detail = strings.TrimSuffix(strings.TrimSpace(rest[j+2:]), ")")
					rest = rest[:j]
				}
				for _, p := range strings.Split(rest, ",") {
					if p = strings.Trim(strings.TrimSpace(p), "`"); p != "" {
						o.Paths = append(o.Paths, p)
					}
				}
			}
		} else if title, why, ok := strings.Cut(mt[3], " -- "); ok {
			o.Title, o.Detail = title, strings.TrimSpace(why)
		}
		o.Title = strings.TrimSpace(o.Title)
		outcomes = append(outcomes, o)
	}
	return outcomes
}

// Run applies plan: a worktree per repo, one model run with file tools
// confined to them, then a commit and a publish per repo that changed.
// It writes apply-input.md, apply.md (the model's output), and apply.json
// (the result) into the plan's directory.
func (a *Applier) Run(ctx context.Context, plan *Plan) (*ApplyResult, error) {
	res := &ApplyResult{Date: plan.Date, Changes: []Landed{}}
	for _, r := range plan.Routes {
		if r.Skip != "" {
			res.Outcomes = append(res.Outcomes, Outcome{Kind: r.Kind, Title: r.Title, Detail: r.Skip})
		}
	}
	if plan.Ready() == 0 {
		return res, a.save(plan, res)
	}

	var wts []*review.Worktree
	cleanup := func() {
		for _, w := range wts {
			_ = w.Remove(ctx, false)
		}
	}
	for i, repo := range plan.Repos {
		w, err := review.AddWorktree(ctx, repo, plan.Worktrees[i], plan.Branch)
		if err != nil {
			cleanup()
			return nil, err
		}
		wts = append(wts, w)
	}

	input := a.BuildInput(plan, wts)
	if err := os.WriteFile(filepath.Join(plan.Dir, "apply-input.md"), []byte(input), 0o644); err != nil {
		cleanup()
		return nil, err
	}
	dirs := make([]string, len(wts))
	for i, w := range wts {
		dirs[i] = w.Dir
	}
	out, usage, err := a.Editor.Edit(ctx, a.Model, input, dirs)
	res.Usage = usage
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("apply model: %w", err)
	}
	if err := os.WriteFile(filepath.Join(plan.Dir, "apply.md"), []byte(out), 0o644); err != nil {
		cleanup()
		return nil, err
	}

	reported := map[string]bool{}
	modelOutcomes := ParseOutcomes(out)
	for _, o := range modelOutcomes {
		reported[o.Kind+":"+o.Title] = true
	}
	res.Outcomes = append(res.Outcomes, modelOutcomes...)
	for _, r := range plan.Routes {
		if r.Skip == "" && !reported[r.Kind+":"+r.Title] {
			res.Outcomes = append(res.Outcomes, Outcome{Kind: r.Kind, Title: r.Title,
				Detail: "the apply model reported nothing for it; check the diff"})
		}
	}

	m := a.mapping(wts)
	var changed []*review.Worktree
	paths := map[*review.Worktree][]string{}
	for _, w := range wts {
		p, err := w.Changed(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w (worktree kept)", w.Dir, err)
		}
		if len(p) == 0 {
			if err := w.Remove(ctx, false); err != nil {
				fmt.Fprintf(a.out(), "recall: %v\n", err)
			}
			continue
		}
		changed = append(changed, w)
		paths[w] = p
	}

	var failed []string
	for _, w := range changed {
		l := a.land(ctx, plan, res, m, w, paths[w], len(changed) == 1)
		if l.Error != "" {
			failed = append(failed, w.Repo)
		}
		res.Changes = append(res.Changes, l)
	}
	_ = os.Remove(filepath.Join(plan.Dir, "worktrees")) // only when empty
	if err := a.save(plan, res); err != nil {
		return res, err
	}
	if len(failed) > 0 {
		return res, fmt.Errorf("change not published for %s; see apply.json", strings.Join(failed, ", "))
	}
	return res, nil
}

// land commits one worktree's change and publishes it. A failed check
// leaves the worktree staged for a look; a failed publish keeps the
// committed branch.
func (a *Applier) land(ctx context.Context, plan *Plan, res *ApplyResult, m mapping, w *review.Worktree,
	changed []string, only bool) Landed {
	l := Landed{Repo: w.Repo, Branch: w.Branch, Base: w.Base, Proposals: []string{}}

	// The Write tool can't set modes, and a graduated script must run.
	if bin, bw := m.at(review.Resolve(a.Bin)); bw == w {
		for _, rel := range changed {
			path := filepath.Join(w.Dir, rel)
			if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && within(path, bin) {
				_ = os.Chmod(path, 0o755)
			}
		}
	}

	isChanged := map[string]bool{}
	for _, rel := range changed {
		isChanged[rel] = true
	}
	var lines []string
	var applied []Outcome
	for _, o := range res.Outcomes {
		if !o.Applied {
			continue
		}
		mine := only
		var rels []string
		for _, p := range o.Paths {
			if p = review.Resolve(p); within(p, w.Dir) {
				mine = true
				// Name only files that really changed.
				if rel, _ := filepath.Rel(w.Dir, p); isChanged[rel] {
					rels = append(rels, "`"+rel+"`")
				}
			}
		}
		if !mine {
			continue
		}
		applied = append(applied, o)
		l.Proposals = append(l.Proposals, o.Title)
		line := fmt.Sprintf("- applied %s: %s", o.Kind, o.Title)
		if len(rels) > 0 {
			line += " (" + strings.Join(rels, ", ") + ")"
		}
		lines = append(lines, line)
	}

	diff, err := w.Stage(ctx)
	if err != nil {
		l.Error = err.Error()
		return l
	}
	if a.Check != "" {
		cmd := exec.CommandContext(ctx, "sh", "-c", a.Check)
		cmd.Dir = w.Dir
		cmd.Stdin = strings.NewReader(diff)
		if out, err := cmd.CombinedOutput(); err != nil {
			l.Error = fmt.Sprintf("learn.check failed (%v); the change is staged in %s: %s", err, w.Dir, strings.TrimSpace(string(out)))
			return l
		}
	}

	title := "Apply learn proposals from " + plan.Date
	if err := w.Commit(ctx, title, strings.Join(lines, "\n")); err != nil {
		l.Error = err.Error()
		return l
	}
	l.Commit, _ = w.Head(ctx)
	where, err := a.Publisher.Publish(ctx, review.Change{Worktree: w, Title: title, Body: a.body(plan, m.public[w], lines, applied)})
	if err != nil {
		l.Error = err.Error()
	}
	l.Where = where
	if rerr := w.Remove(ctx, true); rerr != nil {
		fmt.Fprintf(a.out(), "recall: %v\n", rerr)
	}
	return l
}

// body is the review's description: what was applied here, and the
// proposals behind it unless the repo is public.
func (a *Applier) body(plan *Plan, public bool, lines []string, applied []Outcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Proposals from `recall learn reduce` for %s, applied by `recall learn apply` for review.\n\n", plan.Date)
	if len(lines) == 0 {
		b.WriteString("- the apply model reported no proposals for this repo; review the diff\n")
	}
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	if public {
		b.WriteString("\nThis repo is public, so the proposals behind the change stay on the machine that ran the apply.\n")
		return b.String()
	}
	var blocks []string
	for _, o := range applied {
		for _, r := range plan.Routes {
			if r.Kind == o.Kind && r.Title == o.Title {
				blocks = append(blocks, r.Text)
				break
			}
		}
	}
	if len(blocks) > 0 {
		b.WriteString("\n<details><summary>Proposals</summary>\n\n")
		b.WriteString(strings.Join(blocks, "\n\n"))
		b.WriteString("\n\n</details>\n")
	}
	return b.String()
}

func (a *Applier) save(plan *Plan, res *ApplyResult) error {
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(plan.Dir, "apply.json"), append(data, '\n'), 0o644)
}

func (a *Applier) out() io.Writer {
	if a.Out != nil {
		return a.Out
	}
	return io.Discard
}

// LatestRun returns the newest dated directory under root that holds a
// proposals.md, the default an apply reads.
func LatestRun(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	for i := len(entries) - 1; i >= 0; i-- { // ReadDir sorts by name, so dates sort by time
		e := entries[i]
		if !e.IsDir() || !datePattern.MatchString(e.Name()) {
			continue
		}
		if _, err := time.Parse("2006-01-02", e.Name()); err != nil {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "proposals.md")); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("no reduce run with a proposals.md under %s; run `recall learn reduce` first", root)
}
