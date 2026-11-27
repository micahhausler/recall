package learn

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

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
	// Replaces is the id of the rule a reword, retire, or graduate changes
	// ("- replaces:"). Candidates and Evidence are the candidate ids and
	// the session ids under "- evidence:".
	Replaces   string
	Candidates []string
	Evidence   []string
	// Enforce, Hook, and Permission say how a graduate wants its rule
	// enforced ("- enforce:", "- hook:", "- permission:").
	Enforce, Hook, Permission string
	// Displaces is the id of the rule an add removes to stay within its
	// target's budget ("- displaces:").
	Displaces string
	// Text is the whole block, heading included.
	Text string
}

var fieldRE = regexp.MustCompile(`^- ([a-z]+):\s?(.*)$`)

var ruleIDRE = regexp.MustCompile(`r-\d+`)

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
			case field == "enforce" && cur.Enforce == "":
				cur.Enforce = v
			case field == "hook" && cur.Hook == "":
				cur.Hook = v
			case field == "permission" && cur.Permission == "":
				cur.Permission = v
			case field == "displaces" && cur.Displaces == "":
				cur.Displaces = ruleIDRE.FindString(v)
			case field == "replaces" && cur.Replaces == "":
				cur.Replaces = ruleIDRE.FindString(v)
			case field == "evidence" && cur.Evidence == nil && cur.Candidates == nil:
				cur.Candidates, cur.Evidence = evidenceIDs(v)
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

// evidenceIDs reads an evidence line's candidate and session ids, strictly.
// A candidate id counts only outside parentheses; a parenthesized note (the
// shape ExpandEvidence writes, like "c-0001 (tool-derived repeats; sessions
// s-a, s-b)") contributes only the tokens after its "sessions" label; and a
// rule id (an ablation retire cites one) is never evidence. Everything else
// on the line stays out of the proposal, so no label leaks into
// Review.Evidence and the ledger's lineage.
func evidenceIDs(v string) (candidates, sessions []string) {
	depth := 0
	inSessions := false
	var tok strings.Builder
	emit := func() {
		id := tok.String()
		tok.Reset()
		switch {
		case id == "":
		case ruleIDRE.FindString(id) == id: // a rule id, dropped anywhere
		case depth > 0:
			switch {
			case id == "sessions" || id == "session":
				inSessions = true
			case inSessions && id != "and":
				sessions = append(sessions, id)
			}
		case candidateIDRE.MatchString(id):
			candidates = append(candidates, id)
		case id != "sessions" && id != "session" && id != "and":
			sessions = append(sessions, id)
		}
	}
	for _, r := range v {
		switch {
		case r == '(':
			emit()
			depth++
			inSessions = false
		case r == ')':
			emit()
			if depth > 0 {
				depth--
			}
			inSessions = false
		case strings.ContainsRune(", `;", r):
			emit()
		default:
			tok.WriteRune(r)
		}
	}
	emit()
	return candidates, sessions
}

// Applier stages a reduce's proposals as edits in fresh worktrees, one per
// repo they touch, and hands each changed repo to a review publisher.
// Nothing it does writes a live file.
type Applier struct {
	// Gates are the checks and config the apply shares with the reduce: the
	// targets, repos, budget, reviews, and evidence requirements. All paths
	// in them are live, absolute paths.
	Gates
	// Editor makes the edits with file tools confined to the worktrees.
	// When it is nil, Runner answers with no tools, and the binary writes
	// the files its answer returns (apply_text.go).
	Editor model.Editor
	Runner model.Runner
	Model  string
	// Effort is the level the apply model runs at; empty for the harness
	// default.
	Effort    string
	Publisher review.Publisher
	// Public lists live paths shared publicly (keep.public).
	Public []string
	// Check, when set, is a shell command run in each changed worktree with
	// the staged diff on stdin; a nonzero exit holds that repo's change back.
	Check string
	// OwnerInstructions are extensions.learn-apply's text.
	OwnerInstructions string
	Clock             clock.Clock
	Out               io.Writer
}

// Route is one proposal's place in a plan.
type Route struct {
	Proposal
	// ID is the route's stable id within its plan. Reviews and outcomes are
	// keyed on it, carried through the apply, so nothing downstream depends
	// on the apply model echoing a title exactly.
	ID string
	// Edit are the live files the proposal changes; Script is a graduate's
	// new script.
	Edit   []string
	Script string
	// Enforce is how a graduate's rule is enforced: a script the agent
	// runs, or a hook or permission rule the binary adds to Settings, the
	// live settings file for this route's scope: learn.settings for a
	// global rule, or the repo's own .claude/settings.json for a rule that
	// lives in a repo's context file.
	Enforce  Enforcement
	Settings string
	// Models are the models that answered in the sessions the proposal
	// cites, by the reduce run's evidence record.
	Models []string
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

// route returns the plan's route with that id, or nil.
func (p *Plan) route(id string) *Route {
	if id == "" {
		return nil
	}
	for i := range p.Routes {
		if p.Routes[i].ID == id {
			return &p.Routes[i]
		}
	}
	return nil
}

// allowed reports whether a live file is one the plan's routes may change.
func (p *Plan) allowed() func(live string) bool {
	files := map[string]bool{}
	for _, f := range p.files() {
		files[f] = true
	}
	var skillDirs []string
	for _, r := range p.Routes {
		if r.Skip == "" && r.Kind == "skill" && r.Name != "" && len(r.Edit) == 1 {
			skillDirs = append(skillDirs, filepath.Dir(r.Edit[0]))
		}
	}
	return func(live string) bool {
		if files[live] {
			return true
		}
		for _, dir := range skillDirs {
			if within(live, dir) {
				return true
			}
		}
		return false
	}
}

// files are the live files the plan's routes may change, each once.
func (p *Plan) files() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range p.Routes {
		if r.Skip != "" {
			continue
		}
		for _, path := range append(append([]string{}, r.Edit...), r.Script) {
			if path != "" && !seen[path] {
				seen[path] = true
				out = append(out, path)
			}
		}
	}
	return out
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
	Applied bool   `json:"applied"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	// Route is the plan route the outcome answers, matched by the binary:
	// exactly by kind and title, or by the most alike title of the same
	// kind, since the model may echo a title reworded.
	Route  string   `json:"route,omitempty"`
	Detail string   `json:"detail,omitempty"`
	Paths  []string `json:"paths,omitempty"`
}

// Landed is one repo's change and where it went for review.
type Landed struct {
	Repo      string   `json:"repo"`
	Branch    string   `json:"branch"`
	Base      string   `json:"base"`
	Commit    string   `json:"commit,omitempty"`
	Where     string   `json:"where,omitempty"`
	Proposals []string `json:"proposals"`
	// Routes are the plan route ids behind Proposals, in the same order,
	// and Landed holds, per route id, the rule text an add or reword
	// actually put in this change's staged diff. Commits holds, per route
	// id, the commit that carries the proposal: the change is one commit
	// per proposal, so the owner can drop one before merging.
	Routes  []string          `json:"routes,omitempty"`
	Landed  map[string]string `json:"landed,omitempty"`
	Commits map[string]string `json:"commits,omitempty"`
	Error   string            `json:"error,omitempty"`
}

// ApplyResult is a finished apply, saved as apply.json beside proposals.md.
// Model and Effort are what the apply asked for, and ModelID the model the
// harness resolved.
type ApplyResult struct {
	Date     string      `json:"date"`
	Model    string      `json:"model,omitempty"`
	Effort   string      `json:"effort,omitempty"`
	ModelID  string      `json:"model_id,omitempty"`
	Changes  []Landed    `json:"changes"`
	Outcomes []Outcome   `json:"outcomes"`
	Usage    model.Usage `json:"-"`
}

// ErrApplied is a run directory an earlier apply already finished.
var ErrApplied = errors.New("already applied")

var (
	datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// scriptPattern is a graduated script's whole file name. It ends up in
	// a hook command, so it holds nothing a shell reads as more than a name.
	scriptPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	// plainPath is a path a shell reads as one word with no quoting.
	plainPath = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
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
	evidence, err := LoadRunEvidence(dir)
	if err != nil {
		return nil, err
	}
	replays, err := LoadReplays(dir)
	if err != nil {
		return nil, err
	}
	p := &Plan{Dir: dir, Date: filepath.Base(dir)}
	if !datePattern.MatchString(p.Date) {
		p.Date = a.Clock.Now().Format("2006-01-02")
	}
	p.Branch = "learn-" + p.Date

	props := ParseProposals(string(md))
	// A proposal may start a repo's notes, as the reduce checked it.
	a.addProposedRepoNotes(props, a.norm)

	targets := map[string]bool{}
	for _, t := range a.Targets {
		targets[a.norm(t)] = true
	}
	for _, repo := range a.Repos {
		for _, f := range repo.Files {
			targets[a.norm(f.Path)] = true
		}
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
	ignoredAt := func(path string) bool {
		return review.IgnoredLive(ctx, path)
	}
	use := func(root string) {
		for _, r := range p.Repos {
			if r == root {
				return
			}
		}
		p.Repos = append(p.Repos, root)
	}

	budget := a.budgetCheck()
	textSize := textBudget()
	for i, prop := range props {
		r := Route{ID: fmt.Sprintf("p-%02d", i+1), Proposal: prop, Skip: a.reviewed(prop), Models: evidence.models(prop.Candidates)}
		if r.Skip == "" && a.unsupported(prop, evidence) {
			r.Skip = "none of its evidence is an observation the owner typed (learn.require_owner_evidence)"
		}
		if r.Skip == "" {
			// The reduce run's own replay record: an add whose replay showed
			// the rule changing nothing stays held back.
			r.Skip = replayHold(replays, prop)
		}
		if r.Skip == "" {
			r.Skip = a.scopeProblem(prop, a.norm(prop.Target), evidence)
		}
		if r.Skip == "" && repoNoteRemote(a.RepoNotes, a.norm(prop.Target)) != "" {
			r.Skip = publicNote(a.norm(prop.Target), a.Public, review.Resolve)
		}
		// Placement is enforced, not advised: a global rule earned in one
		// repo belongs in that repo's own file, where only its sessions pay
		// for it.
		if r.Skip == "" {
			if repo := a.narrowScope(prop, a.norm(prop.Target), evidence); repo != nil {
				r.Skip = fmt.Sprintf("it targets a global file, and every session behind it ran in %s; it belongs in %s", repo.Name(a.Home), repo.home())
			}
		}
		if r.Skip == "" {
			r.Skip = a.harnessScope(prop, a.norm(prop.Target), evidence)
		}
		if r.Skip == "" {
			a.route(&r, targets, taken, rootOf, ignoredAt)
		}
		if r.Skip == "" && a.textMode() {
			if why := textSize(a.textPaths(r)); why != "" {
				r.Skip = why
				r.Edit, r.Script = nil, ""
			}
		}
		// Last, so only an add that is going ahead counts against the ones
		// after it; the shared check skips a held-back proposal itself.
		if why := budget(prop, a.norm(prop.Target), r.Skip != ""); why != "" {
			r.Skip = why
			r.Edit, r.Script = nil, ""
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

// rule returns the ledger rule with that id, or nil.
func (a *Applier) rule(id string) *Rule {
	rules := a.rules()
	for i := range rules {
		if rules[i].ID == id {
			return &rules[i]
		}
	}
	return nil
}

// replaces says why a reword, retire, or graduate can't go ahead, if it
// can't: it must name, with "- replaces:", a rule on the ledger that lives
// in one of the targets it edits.
func (a *Applier) replaces(prop Proposal, targets ...string) string {
	if prop.Replaces == "" {
		return "a " + prop.Kind + " has to name the ledger rule it changes (- replaces:)"
	}
	for _, rule := range a.rules() {
		if rule.ID != prop.Replaces {
			continue
		}
		at := review.Resolve(rule.Target)
		for _, t := range targets {
			if at == t {
				return ""
			}
		}
		return fmt.Sprintf("it replaces %s, which is in %s, not in its target", rule.ID, rule.Target)
	}
	return fmt.Sprintf("it replaces %s, which isn't a rule on the ledger", prop.Replaces)
}

// route sets the live files r changes, or why it can't be applied.
func (a *Applier) route(r *Route, targets, taken map[string]bool, rootOf func(string) string, ignoredAt func(string) bool) {
	prop := r.Proposal
	switch prop.Kind {
	case "add", "reword", "retire":
		t := a.norm(prop.Target)
		if prop.Target == "" || !targets[t] {
			r.Skip = fmt.Sprintf("target %q isn't in learn.targets", prop.Target)
			break
		}
		// A reword or retire changes a rule the owner already reviewed, which
		// is why it needs no new evidence. So it has to name that rule: text
		// that replaces nothing is a new rule under another name.
		if prop.Kind != "add" {
			if why := a.replaces(prop, t); why != "" {
				r.Skip = why
				break
			}
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
			// A changed skill is an existing skill file, nothing else: the
			// target is the model's text, and must not reach any other path.
			t := a.norm(prop.Target)
			if st, err := os.Stat(t); filepath.Base(t) != "SKILL.md" || err != nil || !st.Mode().IsRegular() {
				r.Skip = fmt.Sprintf("skill target %q isn't an existing SKILL.md; a new skill needs a name", prop.Target)
				break
			}
			r.Edit = []string{t}
		default:
			r.Skip = "a skill proposal names neither a new skill nor the skill it changes"
		}
	case "graduate":
		e, why := parseEnforcement(prop.Enforce, prop.Hook, prop.Permission)
		if why != "" {
			r.Skip = why
			break
		}
		r.Enforce = e
		if e.Kind != "permission" {
			name := filepath.Base(strings.Trim(strings.TrimSpace(prop.Target), "`"))
			if prop.Target == "" || !scriptPattern.MatchString(name) {
				r.Skip = fmt.Sprintf("graduate target %q doesn't name a script", prop.Target)
				break
			}
			r.Script = review.Resolve(filepath.Join(a.Bin, name))
		}
		// A graduate turns an existing reviewed rule into enforcement, so it
		// must name that rule, and the rule must live in one of the targets
		// the graduate edits; enforcement that replaces nothing is a new rule
		// that skipped review under another kind. The named rule's own file
		// decides the graduate's scope: a rule in a repo's context file
		// graduates into that repo, a global one into learn.targets.
		if prop.Replaces == "" {
			r.Skip = "a " + prop.Kind + " has to name the ledger rule it changes (- replaces:)"
			break
		}
		rule := a.rule(prop.Replaces)
		if rule == nil {
			r.Skip = fmt.Sprintf("it replaces %s, which isn't a rule on the ledger", prop.Replaces)
			break
		}
		repo := repoOf(a.Repos, review.Resolve(rule.Target))
		if repo != nil && repo.RepoNote {
			r.Skip = fmt.Sprintf("its rule lives in the repo note for %s, and recall changes nothing in that repo, its settings included", repo.Remote)
			break
		}
		if repo == nil {
			gts := make([]string, 0, len(a.Targets))
			for _, t := range a.Targets {
				gts = append(gts, a.norm(t))
			}
			if why := a.replaces(prop, gts...); why != "" {
				r.Skip = why
				break
			}
		}
		settings := ""
		if e.Kind != enforceScript {
			// A repo rule's enforcement goes in that repo's own settings
			// file, so only its sessions carry the hook or permission rule;
			// a global rule's needs learn.settings.
			switch {
			case repo != nil:
				settings = review.Resolve(filepath.Join(repo.Root, ".claude", "settings.json"))
			case a.Settings == "":
				r.Skip = "learn.settings isn't set, so there is no settings file to put a " + e.Kind + " in"
			case ignoredAt(review.Resolve(a.Settings)):
				// Likely a generated file a tool overwrites; an edit there
				// could not be reviewed or would not survive.
				r.Skip = fmt.Sprintf("learn.settings (%s) is a git-ignored file, likely generated; point learn.settings at a tracked layer your generator merges", a.Settings)
			default:
				settings = review.Resolve(a.Settings)
			}
			if r.Skip != "" {
				break
			}
			if data, err := os.ReadFile(settings); err == nil {
				if _, err := parseObject(data); err != nil {
					r.Skip = fmt.Sprintf("%s isn't a JSON object the binary can add to: %v", settings, err)
					break
				}
			}
		}
		// The prose rule it retires: the repo rule's own file, or any of the
		// global targets.
		if repo != nil {
			r.Edit = append(r.Edit, review.Resolve(rule.Target))
		} else {
			for _, t := range a.Targets {
				if t := a.norm(t); rootOf(t) != "" {
					r.Edit = append(r.Edit, t)
				}
			}
		}
		if settings != "" {
			r.Edit = append(r.Edit, settings)
			r.Settings = settings
		}
	default:
		r.Skip = fmt.Sprintf("unknown kind %q", prop.Kind)
	}
	if r.Skip == "" {
		need := r.Edit
		if r.Script != "" {
			need = []string{r.Script}
		}
		if r.Enforce.Kind == "hook" || r.Enforce.Kind == "permission" {
			need = append(append([]string{}, need...), r.Settings)
			if r.Script == "" {
				need = need[len(need)-1:]
			}
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
	if dir == string(filepath.Separator) {
		return filepath.IsAbs(path)
	}
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
	input, _ := a.buildInput(plan, wts)
	return input
}

// buildInput is BuildInput, plus, in text mode, the files the input shows
// and the binary accepts back.
func (a *Applier) buildInput(plan *Plan, wts []*review.Worktree) (string, *textFiles) {
	m := a.mapping(wts)
	var b strings.Builder
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
	all := append([]string{}, a.Targets...)
	for _, repo := range a.Repos {
		for _, f := range repo.Files {
			all = append(all, f.Path)
		}
	}
	for _, t := range all {
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
		switch r.Enforce.Kind {
		case "hook":
			fmt.Fprintf(&b, "- enforced: the binary adds this script to the harness settings as a %s hook after you finish; "+
				"write the script, remove the prose rule, and leave no pointer to it\n", strings.TrimSpace(r.Enforce.Event+" "+r.Enforce.Matcher))
		case "permission":
			fmt.Fprintf(&b, "- enforced: the binary adds the permission rule \"%s %s\" to the harness settings after you finish; "+
				"only remove the prose rule, and report the target file you changed\n", r.Enforce.Verb, r.Enforce.Rule)
			continue
		}
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

	// The text of each rule an add displaces, so the model removes the
	// right bullet.
	var displaced []string
	for _, r := range plan.Routes {
		if r.Skip != "" || r.Displaces == "" {
			continue
		}
		for _, rule := range a.rules() {
			if rule.ID == r.Displaces {
				displaced = append(displaced, fmt.Sprintf("Rule[%s]: %s", rule.ID, rule.Text))
			}
		}
	}
	if len(displaced) > 0 {
		b.WriteString("\n# DISPLACED RULES\n\n" + strings.Join(displaced, "\n") + "\n")
	}
	if !a.textMode() {
		return applyPrompt + b.String(), nil
	}
	tf := a.textFiles(plan, m)
	tf.pickMarker(b.String())
	tf.render(&b)
	return strings.ReplaceAll(applyTextPrompt, defaultMarker, tf.marker) + b.String(), tf
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

// titleSimilarity is how alike a model outcome's title and a route's title
// must be, by their words, to be the same proposal when nothing matches
// exactly.
const titleSimilarity = 0.5

// matchOutcomes links the model's outcome lines back to the plan's routes:
// first by kind and title exactly, then by the most alike title of the same
// kind, best pair first. The model may echo a reworded title, and nothing
// downstream should hang on the echo.
func matchOutcomes(routes []Route, outcomes []Outcome) {
	taken := map[string]bool{}
	open := func(r *Route) bool { return r.Skip == "" && !taken[r.ID] }
	for i := range outcomes {
		o := &outcomes[i]
		for j := range routes {
			if r := &routes[j]; open(r) && r.Kind == o.Kind && r.Title == o.Title {
				o.Route = r.ID
				taken[r.ID] = true
				break
			}
		}
	}
	type pair struct {
		oi, rj int
		sim    float64
	}
	var pairs []pair
	for i := range outcomes {
		if outcomes[i].Route != "" {
			continue
		}
		for j := range routes {
			if r := &routes[j]; open(r) && r.Kind == outcomes[i].Kind {
				if sim := similarity(outcomes[i].Title, r.Title); sim >= titleSimilarity {
					pairs = append(pairs, pair{i, j, sim})
				}
			}
		}
	}
	sort.SliceStable(pairs, func(a, b int) bool { return pairs[a].sim > pairs[b].sim })
	for _, p := range pairs {
		if o := &outcomes[p.oi]; o.Route == "" && open(&routes[p.rj]) {
			o.Route = routes[p.rj].ID
			taken[routes[p.rj].ID] = true
		}
	}
}

// stagedSettings is one live settings file as staged in its worktree.
type stagedSettings struct {
	at string
	wt *review.Worktree
}

// settingsSession owns the staged settings files for one apply run and
// holds the run's invariant in one place: nothing a model writes to a
// settings file survives. Each hook or permission route names its own live
// settings file: learn.settings for a global rule, a repo's
// .claude/settings.json for a repo's. Every one of them, plus
// learn.settings whenever it is staged, is the binary's to edit, and is put
// back before the binary makes its own edits: preflight resets after its
// trial edits, and reapply resets before adding the applied routes' edits.
type settingsSession struct {
	a     *Applier
	m     mapping
	files map[string]stagedSettings
}

// stageSettings collects the staged settings files that learn.settings and
// the plan's routes name.
func (a *Applier) stageSettings(plan *Plan, m mapping) *settingsSession {
	s := &settingsSession{a: a, m: m, files: map[string]stagedSettings{}}
	if a.Settings != "" {
		if at, wt := m.at(review.Resolve(a.Settings)); wt != nil {
			s.files[review.Resolve(a.Settings)] = stagedSettings{at, wt}
		}
	}
	for _, r := range plan.Routes {
		if r.Skip == "" && r.Settings != "" {
			if at, wt := m.at(r.Settings); wt != nil {
				s.files[r.Settings] = stagedSettings{at, wt}
			}
		}
	}
	return s
}

// at is the staged path of the route's settings file.
func (s *settingsSession) at(r *Route) (string, error) {
	f, ok := s.files[r.Settings]
	if !ok {
		return "", errors.New("the settings file isn't in what was staged")
	}
	return f.at, nil
}

// reset puts every staged settings file back to its committed state.
func (s *settingsSession) reset(ctx context.Context) error {
	for _, f := range s.files {
		rel, err := filepath.Rel(f.wt.Dir, f.at)
		if err != nil {
			return err
		}
		if err := f.wt.Restore(ctx, rel); err != nil {
			return err
		}
	}
	return nil
}

// preflight tries each hook or permission edit on its staged settings file
// before the model runs, then resets. One the binary can't make is held
// back now, while nothing has removed the prose rule it would have
// replaced.
func (s *settingsSession) preflight(ctx context.Context, plan *Plan, res *ApplyResult) error {
	for i := range plan.Routes {
		r := &plan.Routes[i]
		if r.Skip != "" || (r.Enforce.Kind != "hook" && r.Enforce.Kind != "permission") {
			continue
		}
		at, err := s.at(r)
		if err == nil {
			err = Enforce(at, r.Enforce, s.a.hookCommand(r.Script))
		}
		if err != nil {
			r.Skip = "the binary can't add it to the settings file: " + err.Error()
			res.Outcomes = append(res.Outcomes, Outcome{Kind: r.Kind, Title: r.Title, Route: r.ID, Detail: r.Skip})
		}
	}
	return s.reset(ctx)
}

// reapply runs after the model. The settings files are the binary's to
// edit, and only in the shapes Enforce writes: whatever the model did to
// them is undone, then each applied proposal's hook or permission rule is
// added, and a graduated hook's script must pass the tripwire.
func (s *settingsSession) reapply(ctx context.Context, plan *Plan, res *ApplyResult) error {
	if err := s.reset(ctx); err != nil {
		return err
	}
	for _, r := range plan.Routes {
		if r.Skip != "" || (r.Enforce.Kind != "hook" && r.Enforce.Kind != "permission") {
			continue
		}
		for i := range res.Outcomes {
			o := &res.Outcomes[i]
			if !o.Applied || o.Route != r.ID {
				continue
			}
			at, err := s.at(&r)
			if err == nil {
				err = Enforce(at, r.Enforce, s.a.hookCommand(r.Script))
			}
			if err == nil && r.Script != "" {
				err = s.a.hookScriptOK(s.m, r.Script)
			}
			if err != nil {
				// The model has already removed the prose rule, so publishing
				// would retire a rule with nothing enforcing it. Stop the run.
				return fmt.Errorf("graduate %q: %w; nothing was published", r.Title, err)
			}
			o.Paths = append(o.Paths, at)
		}
	}
	return nil
}

// Run applies plan: a worktree per repo, one model run with file tools
// confined to them (or, in text mode, with none, the binary writing its
// answer), then a commit and a publish per repo that changed.
// It writes apply-input.md, apply.md (the model's output), and apply.json
// (the result) into the plan's directory.
func (a *Applier) Run(ctx context.Context, plan *Plan) (*ApplyResult, error) {
	res := &ApplyResult{Date: plan.Date, Model: a.Model, Effort: a.Effort, Changes: []Landed{}}
	for _, r := range plan.Routes {
		if r.Skip != "" {
			res.Outcomes = append(res.Outcomes, Outcome{Kind: r.Kind, Title: r.Title, Route: r.ID, Detail: r.Skip})
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
		// A publisher that opens the change as someone else, like a bot,
		// says so before the first commit, so the commits are theirs too.
		if c, ok := a.Publisher.(review.Committer); ok {
			w.AuthorName, w.AuthorEmail, err = c.Committer(ctx, w)
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("%s: %w", repo, err)
			}
		}
	}

	m := a.mapping(wts)
	settings := a.stageSettings(plan, m)
	if err := settings.preflight(ctx, plan, res); err != nil {
		cleanup()
		return nil, err
	}
	if plan.Ready() == 0 {
		cleanup()
		_ = os.Remove(filepath.Join(plan.Dir, "worktrees"))
		return res, a.save(plan, res)
	}

	var modelOutcomes []Outcome
	var err error
	if a.textMode() {
		modelOutcomes, err = a.runText(ctx, plan, wts, res)
	} else {
		modelOutcomes, err = a.runEditor(ctx, plan, wts, res)
	}
	if err != nil {
		cleanup()
		return nil, err
	}
	matchOutcomes(plan.Routes, modelOutcomes)
	reported := map[string]bool{}
	for _, o := range modelOutcomes {
		reported[o.Route] = true
	}
	res.Outcomes = append(res.Outcomes, modelOutcomes...)
	for _, r := range plan.Routes {
		if r.Skip == "" && !reported[r.ID] {
			res.Outcomes = append(res.Outcomes, Outcome{Kind: r.Kind, Title: r.Title, Route: r.ID,
				Detail: "the apply model reported nothing for it; check the diff"})
		}
	}

	if err := settings.reapply(ctx, plan, res); err != nil {
		cleanup()
		return nil, err
	}

	// An applied proposal that names a rule to displace must really remove
	// it: the owner reviews the trade as one diff, and the ledger records
	// the displaced rule as retired by this review. If the displaced text
	// is still a bullet in the staged file, the trade didn't happen, so the
	// run stops with nothing published.
	appliedRoutes := map[string]bool{}
	for _, o := range res.Outcomes {
		if o.Applied {
			appliedRoutes[o.Route] = true
		}
	}
	for _, r := range plan.Routes {
		if r.Skip != "" || r.Displaces == "" || !appliedRoutes[r.ID] {
			continue
		}
		for _, rule := range a.rules() {
			if rule.ID != r.Displaces {
				continue
			}
			at, _ := m.at(review.Resolve(rule.Target))
			if at == "" {
				continue
			}
			if data, err := os.ReadFile(at); err == nil && hasBullet(data, rule.Text) {
				cleanup()
				return nil, fmt.Errorf("%s %q: the rule it displaces (%s: %q) is still in %s; nothing was published",
					r.Kind, r.Title, rule.ID, rule.Text, rule.Target)
			}
		}
	}

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

// runEditor runs the Editor with file tools confined to the worktrees, and
// returns the outcomes it reports.
func (a *Applier) runEditor(ctx context.Context, plan *Plan, wts []*review.Worktree, res *ApplyResult) ([]Outcome, error) {
	if a.Editor == nil {
		return nil, errors.New("apply: no editor or runner to apply with")
	}
	input := a.BuildInput(plan, wts)
	dirs := make([]string, len(wts))
	for i, w := range wts {
		dirs[i] = w.Dir
	}
	// The harness won't let the model edit under a .claude directory, so it
	// edits copies (shadow.go).
	input, shadowRoot, shadows, err := shadowClaudeDirs(input, plan.Dir, dirs)
	if err != nil {
		return nil, err
	}
	if shadowRoot != "" {
		dirs = append(dirs, shadowRoot)
		defer os.RemoveAll(shadowRoot)
	}
	if err := os.WriteFile(filepath.Join(plan.Dir, "apply-input.md"), []byte(input), 0o644); err != nil {
		return nil, err
	}
	out, usage, err := a.Editor.Edit(ctx, a.call(), input, dirs)
	res.Usage, res.ModelID = usage, usage.ModelID
	if err != nil {
		return nil, fmt.Errorf("apply model: %w", err)
	}
	if err := syncBack(shadows); err != nil {
		return nil, err
	}
	out = unshadow(out, shadows)
	if err := os.WriteFile(filepath.Join(plan.Dir, "apply.md"), []byte(out), 0o644); err != nil {
		return nil, err
	}
	return ParseOutcomes(out), nil
}

// runText runs the Runner with no tools, writes the files its answer
// returns, and returns the outcomes it reports.
func (a *Applier) runText(ctx context.Context, plan *Plan, wts []*review.Worktree, res *ApplyResult) ([]Outcome, error) {
	input, tf := a.buildInput(plan, wts)
	if err := os.WriteFile(filepath.Join(plan.Dir, "apply-input.md"), []byte(input), 0o644); err != nil {
		return nil, err
	}
	out, usage, err := a.Runner.Run(ctx, a.call(), input)
	res.Usage, res.ModelID = usage, usage.ModelID
	if err != nil {
		return nil, fmt.Errorf("apply model: %w", err)
	}
	if err := os.WriteFile(filepath.Join(plan.Dir, "apply.md"), []byte(out), 0o644); err != nil {
		return nil, err
	}
	outcomes, err := a.writeAnswer(out, tf)
	if err != nil {
		return nil, fmt.Errorf("apply model: %w", err)
	}
	return outcomes, nil
}

// hookScriptOK checks a graduated hook's script as staged. A hook may only
// block a call, by exiting 2.
func (a *Applier) hookScriptOK(m mapping, script string) error {
	at, _ := m.at(script)
	data, err := os.ReadFile(at)
	if err != nil {
		return fmt.Errorf("the model reported it applied but wrote no script at %s", at)
	}
	return hookScriptTripwire(data)
}

// hookOutputFields are the JSON output fields Claude Code reads from a
// hook to approve or block an action, stop the session, rewrite a tool's
// input or output, or feed text back into the conversation, per
// https://code.claude.com/docs/en/hooks (the current home of
// https://docs.claude.com/en/docs/claude-code/hooks), checked 2026-11:
//
//   - continue, stopReason: stop the session after the hook, with a message
//     the model sees; suppressOutput and systemMessage shape what's shown.
//   - decision, reason: the older top-level form. "approve"/"block" on
//     PreToolUse is deprecated in favor of permissionDecision, but
//     decision: "block" is still current for UserPromptSubmit, PostToolUse,
//     Stop, SubagentStop, and others.
//   - hookSpecificOutput members: permissionDecision ("allow", "deny",
//     "ask", "defer") and permissionDecisionReason approve or block a
//     PreToolUse call; updatedInput rewrites the tool input; additionalContext
//     injects text into the conversation; updatedToolOutput and
//     updatedMCPToolOutput replace what a tool returned; retry lets a denied
//     call be retried (PermissionDenied).
//   - PermissionRequest's decision object: behavior ("allow"/"deny"),
//     updatedInput, updatedPermissions, message, interrupt.
//
// hookEventName isn't listed on its own: it only labels hookSpecificOutput,
// which is, and hook_event_name is in every hook's input. Matching is on
// collapsed lowercase text, longest field first, so each hit names the most
// specific field.
var hookOutputFields = []string{
	"permissiondecisionreason",
	"permissiondecision",
	"updatedmcptooloutput",
	"hookspecificoutput",
	"updatedtooloutput",
	"updatedpermissions",
	"additionalcontext",
	"suppressoutput",
	"systemmessage",
	"updatedinput",
	"stopreason",
	"interrupt",
	"behavior",
	"decision",
	"continue",
	"reason",
	"retry",
}

var (
	// hookEscape is a shell or printf character escape that could spell a
	// field one byte at a time: \xNN, \uNNNN, or octal \NNN.
	hookEscape = regexp.MustCompile(`\\(?:x[0-9a-fA-F]{2}|u[0-9a-fA-F]{4}|[0-7]{1,3})`)
	// hookVerb is a printf conversion, which splices arguments between the
	// literal text around it.
	hookVerb = regexp.MustCompile(`%[-+ #0-9.*]*[a-zA-Z]`)
	// hookB64 is a token long enough to decode into a field name.
	hookB64 = regexp.MustCompile(`[A-Za-z0-9+/_-]{8,}={0,2}`)
)

// hookScriptTripwire refuses a hook script that names any of the harness's
// hook output fields. A script that emits one could approve calls the
// owner's settings would have asked about, or feed text into the session,
// so a mention — whole, or built in an obvious way from fragments (string
// concatenation, printf of the key, character escapes, base64) — is
// refused, and so is any of the words even in a comment: false trips only
// hold a proposal back. This is a tripwire for the obvious case, not a
// proof about what a script does: human review is the control, and the
// owner still reads the script in review.
func hookScriptTripwire(data []byte) error {
	// Decode character escapes, then drop everything but letters and digits:
	// quoting, concatenation, and printf verbs sit between fragments, never
	// inside a field name.
	texts := []string{hookCollapse(hookEscape.ReplaceAllFunc(hookVerb.ReplaceAll(data, nil), func(m []byte) []byte {
		base, digits := 16, m[2:]
		if m[1] != 'x' && m[1] != 'u' {
			base, digits = 8, m[1:]
		}
		if n, err := strconv.ParseUint(string(digits), base, 32); err == nil && n < 128 {
			return []byte{byte(n)}
		}
		return m
	}))}
	for _, tok := range hookB64.FindAll(data, -1) {
		tok = bytes.TrimRight(tok, "=")
		for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
			if dec, err := enc.DecodeString(string(tok)); err == nil {
				texts = append(texts, hookCollapse(dec))
			}
		}
	}
	for _, text := range texts {
		for _, field := range hookOutputFields {
			if strings.Contains(text, field) {
				return fmt.Errorf("its hook script names the harness hook output field %q; a learned hook may only block, by exiting 2", field)
			}
		}
	}
	return nil
}

// hookCollapse lowercases b and keeps only its letters and digits.
func hookCollapse(b []byte) string {
	var out []byte
	for _, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+'a'-'A')
		}
	}
	return string(out)
}

// hookCommand is how a settings file names a graduated script: by its live
// path, written from $HOME when it is under home, so one settings file works
// on machines whose home directories differ.
func (a *Applier) hookCommand(script string) string {
	if script == "" {
		return ""
	}
	// Quoted, so a directory with a space or a shell character in its name
	// stays one word of the command.
	quote := func(s string) string {
		if plainPath.MatchString(s) {
			return s
		}
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	if rel, err := filepath.Rel(review.Resolve(a.Home), script); a.Home != "" && err == nil && !strings.HasPrefix(rel, "..") {
		return `"$HOME"/` + quote(filepath.ToSlash(rel))
	}
	return quote(script)
}

// land commits one worktree's change and publishes it. A failed check
// leaves the worktree staged for a look; a failed publish keeps the
// committed branch.
func (a *Applier) land(ctx context.Context, plan *Plan, res *ApplyResult, m mapping, w *review.Worktree,
	changed []string, only bool) Landed {
	l := Landed{Repo: w.Repo, Branch: w.Branch, Base: w.Base, Proposals: []string{}}

	// The model's writes never set modes, and a graduated script must run.
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
	relsOf := map[string][]string{} // route id -> the changed paths it names
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
					rels = append(rels, rel)
				}
			}
		}
		if !mine {
			continue
		}
		// The proposal's own title, not the model's echo of it, so the
		// review ledger names what the reduce proposed.
		title := o.Title
		if r := plan.route(o.Route); r != nil {
			title = r.Title
		}
		applied = append(applied, o)
		relsOf[o.Route] = rels
		l.Proposals = append(l.Proposals, title)
		l.Routes = append(l.Routes, o.Route)
		line := fmt.Sprintf("- applied %s: %s", o.Kind, title)
		if len(rels) > 0 {
			quoted := make([]string, len(rels))
			for i, rel := range rels {
				quoted[i] = "`" + rel + "`"
			}
			line += " (" + strings.Join(quoted, ", ") + ")"
		}
		lines = append(lines, line)
	}

	// Stage exactly the paths the plan routed, each forced past any ignore
	// rule, so a file the apply wrote never falls out of the change
	// silently. The one exception is an ignored learn.settings file: it is
	// likely generated, so it is never force-added, and the post-commit
	// check below fails the change naming it instead.
	allowed := plan.allowed()
	settingsRels := map[string]bool{}
	learnSettingsRel := ""
	if a.Settings != "" {
		if at, wt := m.at(review.Resolve(a.Settings)); wt == w {
			if rel, err := filepath.Rel(w.Dir, at); err == nil {
				learnSettingsRel = filepath.ToSlash(rel)
				settingsRels[learnSettingsRel] = true
			}
		}
	}
	for _, r := range plan.Routes {
		if r.Skip == "" && r.Settings != "" {
			if at, wt := m.at(r.Settings); wt == w {
				if rel, err := filepath.Rel(w.Dir, at); err == nil {
					settingsRels[filepath.ToSlash(rel)] = true
				}
			}
		}
	}
	var routed []string
	for _, rel := range changed {
		if allowed(filepath.Join(w.Repo, filepath.FromSlash(rel))) || settingsRels[rel] {
			routed = append(routed, rel)
		}
	}
	ignored, err := w.Ignored(ctx, routed)
	if err != nil {
		l.Error = err.Error()
		return l
	}
	var stage, forced []string
	for _, rel := range routed {
		if rel == learnSettingsRel && slices.Contains(ignored, rel) {
			continue // never force-add an ignored learn.settings file
		}
		stage = append(stage, rel)
		if slices.Contains(ignored, rel) {
			forced = append(forced, rel)
		}
	}

	diff, err := w.Stage(ctx, stage)
	if err != nil {
		l.Error = err.Error()
		return l
	}
	l.Landed = landedRules(plan, applied, diff, w.Dir)
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
	// One commit per proposal, so the owner can drop one before merging and
	// each proposal gets its own verdict; the last commit also carries
	// anything the outcomes didn't name, so nothing staged is left behind.
	if err := w.Unstage(ctx); err != nil {
		l.Error = err.Error()
		return l
	}
	l.Commits = map[string]string{}
	staged := map[string]bool{}
	for _, rel := range stage {
		staged[rel] = true
	}
	last := ""
	for i, o := range applied {
		msg := fmt.Sprintf("%s: %s", o.Kind, l.Proposals[i])
		// Only paths the vetted stage covered: an ignored learn.settings
		// file stays out of every commit, per the staging exception above.
		var rels []string
		for _, rel := range relsOf[o.Route] {
			if staged[rel] {
				rels = append(rels, rel)
			}
		}
		var rest []string
		if i == len(applied)-1 {
			rest = stage // sweep anything no outcome named; re-adds are no-ops
		}
		commit, err := w.CommitPaths(ctx, msg, rels, rest)
		if err != nil {
			l.Error = err.Error()
			return l
		}
		if commit == "" {
			// Its file was already carried by an earlier proposal's commit,
			// so the two share a verdict.
			commit = last
		}
		if commit != "" {
			l.Commits[o.Route] = commit
			last = commit
		}
	}
	if len(applied) == 0 {
		// The model reported nothing the binary could link here; commit the
		// diff whole for the owner to read.
		if _, err := w.CommitPaths(ctx, title, nil, stage); err != nil {
			l.Error = err.Error()
			return l
		}
	}
	l.Commit, _ = w.Head(ctx)
	// Every routed path must be in the commit; one an ignore rule dropped
	// anyway fails the change here, named, never silently.
	committed, err := w.Committed(ctx)
	if err != nil {
		l.Error = err.Error()
		return l
	}
	inCommit := map[string]bool{}
	for _, p := range committed {
		inCommit[p] = true
	}
	for _, rel := range routed {
		if !inCommit[rel] {
			l.Error = fmt.Sprintf("%s was routed a change but is missing from the commit; the change is staged in %s", rel, w.Dir)
			return l
		}
	}
	where, err := a.Publisher.Publish(ctx, review.Change{Worktree: w, Title: title, Body: a.body(plan, m.public[w], lines, applied, forced),
		Proposals: l.Proposals})
	if err != nil {
		l.Error = err.Error()
	}
	l.Where = where
	if rerr := w.Remove(ctx, true); rerr != nil {
		fmt.Fprintf(a.out(), "recall: %v\n", rerr)
	}
	return l
}

// hasBullet reports whether data holds text as a top-level bullet, by the
// same scan rules sync reads targets with.
func hasBullet(data []byte, text string) bool {
	want := textHash(strings.TrimSpace(text))
	for _, b := range scanBullets(data) {
		if b.hash == want {
			return true
		}
	}
	return false
}

// hunkStart reads the new side's 1-based starting line from a @@ hunk
// header, or 0 when the header doesn't say.
func hunkStart(header string) int {
	for _, f := range strings.Fields(header) {
		if strings.HasPrefix(f, "+") {
			n, _ := strconv.Atoi(strings.TrimPrefix(strings.SplitN(f, ",", 2)[0], "+"))
			return n
		}
	}
	return 0
}

// newSidePath reads a diff's +++ header: the new side's path with git's b/
// prefix dropped, or "" for a deleted file.
func newSidePath(name string) string {
	if unquoted, err := strconv.Unquote(name); err == nil && strings.HasPrefix(name, `"`) {
		name = unquoted
	}
	if name == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(name, "b/")
}

// addedLines reads from a unified diff the 1-based new-side line numbers
// the added lines landed on, keyed by each file's new-side path.
func addedLines(diff string) map[string]map[int]bool {
	added := map[string]map[int]bool{}
	file := ""
	line := 0
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++ "):
			file, line = newSidePath(strings.TrimPrefix(l, "+++ ")), 0
		case strings.HasPrefix(l, "@@"):
			line = hunkStart(l)
		case file == "" || line == 0:
			// Between a file header and its first hunk, or a deleted file.
		case strings.HasPrefix(l, "+"):
			if added[file] == nil {
				added[file] = map[int]bool{}
			}
			added[file][line] = true
			line++
		case strings.HasPrefix(l, " "):
			line++
		}
	}
	return added
}

// landedRules reads from a change's staged diff the bullet each applied add
// or reword actually landed, keyed by route id. The apply model may tighten
// wording as it lands a rule, so a review's lineage records the text that is
// in the file, not the text the proposal carried; each added bullet goes to
// the proposal whose rule it is most like. dir is the worktree the diff was
// staged in, holding the files the diff's new side describes.
func landedRules(plan *Plan, applied []Outcome, diff, dir string) map[string]string {
	// A hunk alone can't say what an added line is: one that starts inside
	// a code fence shows only the closing fence as context, and one inside
	// frontmatter never shows the opening ---. So each changed file is
	// scanned whole, by the same line classification sync reads a target
	// with, and only the bullets on the diff's added lines count.
	byFile := addedLines(diff)
	var files []string
	for rel := range byFile {
		files = append(files, rel)
	}
	sort.Strings(files)
	var added []string
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			continue // gone from the worktree since the diff was taken
		}
		var s bulletScanner
		for i, line := range strings.Split(string(data), "\n") {
			if text, ok := s.scan(line); ok && byFile[rel][i+1] {
				added = append(added, text)
			}
		}
	}
	if len(added) == 0 {
		return nil
	}
	type pair struct {
		route *Route
		bi    int
		sim   float64
	}
	var pairs []pair
	for _, o := range applied {
		r := plan.route(o.Route)
		if r == nil || (r.Kind != "add" && r.Kind != "reword") {
			continue
		}
		for bi, text := range added {
			if sim := similarity(r.Rule, text); sim >= titleSimilarity {
				pairs = append(pairs, pair{r, bi, sim})
			}
		}
	}
	sort.SliceStable(pairs, func(a, b int) bool { return pairs[a].sim > pairs[b].sim })
	landed := map[string]string{}
	usedBullet := map[int]bool{}
	for _, p := range pairs {
		if landed[p.route.ID] == "" && !usedBullet[p.bi] {
			landed[p.route.ID] = added[p.bi]
			usedBullet[p.bi] = true
		}
	}
	if len(landed) == 0 {
		return nil
	}
	return landed
}

// body is the review's description: what was applied here, and the
// proposals behind it unless the repo is public.
func (a *Applier) body(plan *Plan, public bool, lines []string, applied []Outcome, forced []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Proposals from `recall learn reduce` for %s, applied by `recall learn apply` for review.\n\n", plan.Date)
	if len(lines) == 0 {
		b.WriteString("- the apply model reported no proposals for this repo; review the diff\n")
	}
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	if notes := a.repoNotesIn(plan, applied); len(notes) > 0 {
		fmt.Fprintf(&b, "\nRepo notes change here, each loaded only in %s sessions that start in its repo; recall changes nothing in those repos:\n", harnessList(a.NoteHarnesses))
		for _, line := range notes {
			b.WriteString(line + "\n")
		}
	}
	if len(forced) > 0 {
		b.WriteString("\nAn ignore rule matches these files; they were committed with `git add -f` so the change stays whole. Fix the ignore file if they should stay tracked:\n")
		for _, rel := range forced {
			b.WriteString("- `" + rel + "`\n")
		}
	}
	if public {
		b.WriteString("\nThis repo is public, so the proposals behind the change stay on the machine that ran the apply.\n")
		return b.String()
	}
	var blocks []string
	for _, o := range applied {
		if r := plan.route(o.Route); r != nil {
			blocks = append(blocks, r.Text)
		}
	}
	if len(blocks) > 0 {
		b.WriteString("\n<details><summary>Proposals</summary>\n\n")
		b.WriteString(strings.Join(blocks, "\n\n"))
		b.WriteString("\n\n</details>\n")
	}
	return b.String()
}

// repoNotesIn lists the repos whose repo notes the applied proposals edit.
func (a *Applier) repoNotesIn(plan *Plan, applied []Outcome) []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range applied {
		r := plan.route(o.Route)
		if r == nil {
			continue
		}
		for _, path := range r.Edit {
			if remote := repoNoteRemote(a.RepoNotes, path); remote != "" && !seen[remote] {
				seen[remote] = true
				out = append(out, "- "+remote)
			}
		}
	}
	return out
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

// call is the model and effort the apply runs.
func (a *Applier) call() model.Call { return model.Call{Model: a.Model, Effort: a.Effort} }
