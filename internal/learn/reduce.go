package learn

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/review"
)

// Reducer runs the weekly pass: observations + rules + aging in, proposals
// out. Proposals are written for review, never applied to live files.
type Reducer struct {
	// Gates are the checks and config the reduce shares with the apply: the
	// targets, repos, budget, evidence requirements, and the ledger, whose
	// rules and reviews the checks read.
	Gates
	Runner model.Runner
	Model  string
	// RetireDays is the aging window, default 90.
	RetireDays int
	// WindowDays is how far back the reduce reads observations
	// (learn.window_days); 0 reads RetireDays back. AllWindow reads every
	// observation (`recall learn reduce --window all`). Neither moves
	// retirement, which always uses RetireDays.
	WindowDays int
	AllWindow  bool
	Clock      clock.Clock
	Out        io.Writer
	// OwnerInstructions are the owner's extension text for the reduce
	// (extensions.learn-reduce), appended to its input when set.
	OwnerInstructions string
	// Replay, when set (learn.replay), replays the moment that earned each
	// add before it is proposed, and replays outgrown rules' moments
	// without them; nil leaves replays off.
	Replay *Replayer
	// Candidates is the candidate ledger; the reduce adds the window's new
	// candidates to it and records which ones its proposals cited. Nil
	// starts an empty one.
	Candidates *CandidateLedger
	// OpenCandidates is how many open candidates the last Run gave the
	// model. A run that read none did nothing a later run can't redo.
	OpenCandidates int
	// loads counts the window's sessions that load each scope's targets:
	// "" for the global ones, a repo's root for its own.
	loads map[string]int
}

// size says what a target costs and, with a budget, how much of it is
// used.
func (r *Reducer) size(path string) string {
	cost := fileCost(path)
	if r.Budget > 0 {
		note := ""
		if cost >= r.Budget {
			note = "; over budget: an add here must displace a rule"
		}
		return fmt.Sprintf(" (about %d of %d tokens%s)", cost, r.Budget, note)
	}
	if cost == 0 {
		return ""
	}
	return fmt.Sprintf(" (about %d tokens)", cost)
}

// clusters groups the window's observations into candidates and returns the
// ones the reduce should see, and how many were held back as already used.
func (r *Reducer) clusters(window []SessionObservations) (open []Cluster, used int) {
	if r.Candidates == nil {
		r.Candidates = &CandidateLedger{}
	}
	now := r.Clock.Now()
	cutoff := r.windowCutoff(now)
	// How many of the window's sessions load each target: every one for a
	// global file, and a repo's own for that repo's files.
	r.loads = map[string]int{}
	for _, so := range window {
		if so.Prompts == 0 || so.Day() < cutoff {
			continue
		}
		r.loads[""]++
		for i := range r.Repos {
			if r.Repos[i].holds(so.Scope.Key(), r.Home) {
				r.loads[r.Repos[i].Root]++
			}
		}
	}
	all := r.Candidates.Cluster(withRepeats(window, cutoff, r.RepeatEvidenceSessions), cutoff, now.Format("2006-01-02"))
	r.Candidates.Prune(all)
	return Open(all, r.reviews())
}

func (r *Reducer) retireDays() int {
	if r.RetireDays == 0 {
		return 90
	}
	return r.RetireDays
}

// windowCutoff is the oldest session day the reduce reads observations from.
// Candidates come from this window; aging and review recency keep using
// retireDays, so widening the window retires nothing faster.
func (r *Reducer) windowCutoff(now time.Time) string {
	if r.AllWindow {
		return "" // every day compares at or after an empty cutoff
	}
	days := r.WindowDays
	if days <= 0 {
		days = r.retireDays()
	}
	return now.AddDate(0, 0, -days).Format("2006-01-02")
}

// BuildInput renders the reduce model's input from the window's observations.
func (r *Reducer) BuildInput(window []SessionObservations) string {
	open, used := r.clusters(window)
	return r.buildInput(open, used)
}

// CandidatesText is the CANDIDATES section of the reduce input alone.
func (r *Reducer) CandidatesText(window []SessionObservations) string {
	var b strings.Builder
	open, used := r.clusters(window)
	renderCandidates(&b, open, used)
	return b.String()
}

func (r *Reducer) buildInput(open []Cluster, used int) string {
	var b strings.Builder
	b.WriteString(reducePrompt)

	b.WriteString("\n\n# TARGETS\n\n")
	if len(r.Targets) == 0 && len(r.Repos) == 0 {
		b.WriteString("(none configured)\n")
	}
	if len(r.Repos) > 0 && len(r.Targets) > 0 {
		b.WriteString("Global, loaded in every session:\n")
	}
	for _, t := range r.Targets {
		fmt.Fprintf(&b, "- %s%s\n", t, r.size(t))
	}
	for _, repo := range r.Repos {
		name := repo.Remote
		if name == "" {
			name = homeRelative(repo.Root, r.Home)
		}
		fmt.Fprintf(&b, "\nRepo %s, loaded only in sessions that run there:\n", name)
		for _, f := range repo.Files {
			fmt.Fprintf(&b, "- %s%s", f.Path, r.size(f.Path))
			switch {
			case f.New:
				b.WriteString(" (doesn't exist yet; the repo has no context file)")
			case len(f.Globs) > 0:
				fmt.Fprintf(&b, " (applies to files matching %s)", strings.Join(f.Globs, ", "))
			}
			b.WriteString("\n")
		}
	}

	if r.Skills != "" || r.Bin != "" {
		b.WriteString("\n# PLACES\n\n")
		if r.Skills != "" {
			fmt.Fprintf(&b, "- a new skill goes at %s/<name>/SKILL.md\n", r.Skills)
		}
		if r.Bin != "" {
			fmt.Fprintf(&b, "- a graduated script goes at %s/<name>\n", r.Bin)
		}
		if r.Settings != "" {
			fmt.Fprintf(&b, "- a graduated hook or permission rule goes in %s, added by the binary\n", r.Settings)
		}
	}

	b.WriteString("\n# CURRENT RULES\n\n")
	if len(r.Ledger.Rules) == 0 {
		b.WriteString("(none on the ledger yet)\n")
	}
	for _, rule := range r.Ledger.Rules {
		fmt.Fprintf(&b, "Rule[%s] (%s; created %s", rule.ID, rule.Target, rule.Created)
		if len(rule.Supersedes) > 0 {
			fmt.Fprintf(&b, "; reworded %s", rule.Since)
		}
		if len(rule.Models) > 0 {
			fmt.Fprintf(&b, "; earned on %s", strings.Join(rule.Models, ", "))
		}
		if rule.LastRelevant != "" {
			fmt.Fprintf(&b, "; last relevant %s", rule.LastRelevant)
		}
		if rule.LastViolated != "" {
			fmt.Fprintf(&b, "; last violated %s", rule.LastViolated)
		}
		// What the rule costs: its tokens, in every session that loads it.
		scope := ""
		if repo := repoOf(r.Repos, rule.Target); repo != nil {
			scope = repo.Root
		}
		if n := r.loads[scope]; n > 0 {
			fmt.Fprintf(&b, "; about %d tokens in each of %d session(s)", tokenCost(rule.Text), n)
		}
		fmt.Fprintf(&b, "): %s\n", rule.Text)
	}

	b.WriteString("\n# CANDIDATES\n\n")
	renderCandidates(&b, open, used)

	retireDays := r.retireDays()
	aging := r.Ledger.Age(r.Clock.Now(), retireDays)
	b.WriteString("\n# AGING\n\n")
	if len(aging.Reword) == 0 && len(aging.Retire) == 0 {
		b.WriteString("(nothing stale or violated)\n")
	}
	for _, rule := range aging.Reword {
		fmt.Fprintf(&b, "reword Rule[%s] (last violated %s): %s\n", rule.ID, rule.LastViolated, rule.Text)
	}
	for _, rule := range aging.Retire {
		fmt.Fprintf(&b, "retire Rule[%s] (no relevance in %d days): %s\n", rule.ID, retireDays, rule.Text)
	}

	cutoff := r.Clock.Now().AddDate(0, 0, -retireDays).Format("2006-01-02")
	var shown []string
	for _, rv := range r.reviews() {
		rule := strings.Join(strings.Fields(rv.Rule), " ")
		switch {
		case rv.Verdict == review.Pending:
			shown = append(shown, fmt.Sprintf("- pending since %s: %s: %s: %s", rv.Proposed, rv.Kind, rv.Title, rule))
		case rv.Verdict == review.Closed && rv.Decided >= cutoff:
			shown = append(shown, fmt.Sprintf("- rejected %s: %s: %s: %s", rv.Decided, rv.Kind, rv.Title, rule))
		}
	}
	if len(shown) > 0 {
		b.WriteString("\n# REVIEWS\n\n")
		b.WriteString(strings.Join(shown, "\n") + "\n")
	}

	if strings.TrimSpace(r.OwnerInstructions) != "" {
		b.WriteString("\n# OWNER INSTRUCTIONS\n\n")
		b.WriteString(strings.TrimRight(r.OwnerInstructions, "\n") + "\n")
	}

	b.WriteString("\n# TAKEN NAMES\n\n")
	if len(r.Taken) == 0 {
		b.WriteString("(unknown)\n")
	} else {
		b.WriteString(strings.Join(r.Taken, ", ") + "\n")
	}
	return b.String()
}

// Run writes proposals.md (the model's output) and input.md (what it saw)
// into outDir, so a review has both, and reports what the call cost.
func (r *Reducer) Run(ctx context.Context, window []SessionObservations, outDir string) (string, model.Usage, error) {
	open, used := r.clusters(window)
	r.OpenCandidates = len(open)
	input := r.buildInput(open, used)
	out, usage, err := r.Runner.Run(ctx, r.Model, input)
	if err != nil {
		return "", usage, err
	}
	// The reduce cites candidates; spell out the sessions behind each, and
	// mark the cited candidates as used.
	out = ExpandEvidence(out, open)
	date := filepath.Base(outDir)
	if !datePattern.MatchString(date) {
		date = r.Clock.Now().Format("2006-01-02") // the date an apply of this run records
	}
	props := ParseProposals(out)
	r.Candidates.Cite(props, date)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", usage, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "input.md"), []byte(input), 0o644); err != nil {
		return "", usage, err
	}
	evidence := runEvidence(open)
	if err := evidence.save(outDir); err != nil {
		return "", usage, err
	}
	out, flagged := r.annotate(out, props, evidence)
	if r.Replay != nil {
		// Replay each add that is still going ahead: the moment that earned
		// it, with and without the rule. Then the ablation: each rule earned
		// only on models no recent session ran is replayed without it, and
		// one the current model follows unprompted gets a retire proposal.
		recs := r.Replay.AddReplays(ctx, props, open, flagged, r.Home)
		blocks, abl := r.Replay.Ablation(ctx, r.Ledger.Outgrown(WindowModels(window, r.Clock.Now())), window)
		recs = append(recs, abl...)
		var held []string
		for _, rec := range recs {
			if rec.Held != "" {
				held = append(held, fmt.Sprintf("- %q: %s; the apply holds it back.", rec.Title, rec.Held))
			}
		}
		if len(held) > 0 {
			out += "\n## Replay\n\n" + strings.Join(held, "\n") + "\n"
			if r.Out != nil {
				fmt.Fprintf(r.Out, "warning: %d proposal(s) held back by replay\n", len(held))
			}
		}
		if len(blocks) > 0 {
			out += "\n## Proposals\n\nThe binary adds these from replays (learn.replay):\n\n" + strings.Join(blocks, "\n")
		}
		if len(recs) > 0 {
			if err := saveReplays(outDir, recs); err != nil {
				return "", usage, err
			}
		}
		usage = usage.Add(r.Replay.Usage)
	}
	proposals := filepath.Join(outDir, "proposals.md")
	if err := os.WriteFile(proposals, []byte(out), 0o644); err != nil {
		return "", usage, err
	}
	return proposals, usage, nil
}

// A reduceFinding is one proposal a post-model check holds back: the title
// the replay and later checks skip, and the line its section shows. An
// empty title renders a line without holding anything back.
type reduceFinding struct {
	title string
	line  string
}

// A reduceCheck is one post-model check: the "## " heading its findings
// render under, the findings themselves, and the stderr warning when it
// fires.
type reduceCheck struct {
	section string
	run     func(flagged map[string]bool) []reduceFinding
	warn    func(fs []reduceFinding) string
}

// annotate runs the post-model checks over the parsed proposals, appends
// one section per check that found something, and returns the output with
// the titles held back. The checks run in apply.Plan's order — the budget
// last, over only the proposals no earlier check flagged — so a proposal
// flagged here is one the apply would also hold back, and a replay is
// never spent on a proposal that can't go ahead anyway.
func (r *Reducer) annotate(out string, props []Proposal, evidence RunEvidence) (string, map[string]bool) {
	flagged := map[string]bool{}
	for _, c := range r.checks(out, props, evidence) {
		fs := c.run(flagged)
		if len(fs) == 0 {
			continue
		}
		var lines []string
		for _, f := range fs {
			if f.title != "" {
				flagged[f.title] = true
			}
			lines = append(lines, f.line)
		}
		out += "\n## " + c.section + "\n\n" + strings.Join(lines, "\n") + "\n"
		if r.Out != nil {
			fmt.Fprintf(r.Out, "warning: %s\n", c.warn(fs))
		}
	}
	return out, flagged
}

// checks is the post-model check table; a new check is one more row.
func (r *Reducer) checks(out string, props []Proposal, evidence RunEvidence) []reduceCheck {
	count := func(format string) func([]reduceFinding) string {
		return func(fs []reduceFinding) string { return fmt.Sprintf(format, len(fs)) }
	}
	targets := append([]string{}, r.Targets...)
	for _, repo := range r.Repos {
		for _, f := range repo.Files {
			targets = append(targets, f.Path)
		}
	}
	clashes := NameCollisions(out, r.Taken)
	return []reduceCheck{{
		section: "Already in review",
		warn:    count("%d proposal(s) are already reviewed"),
		run: func(map[string]bool) []reduceFinding {
			var fs []reduceFinding
			for _, p := range props {
				if why := r.reviewed(p); why != "" {
					fs = append(fs, reduceFinding{title: p.Title,
						line: fmt.Sprintf("- %q: %s; the apply skips it.", p.Title, why)})
				}
			}
			return fs
		},
	}, {
		section: "Unsupported proposals",
		warn:    count("%d proposal(s) cite no owner-typed evidence"),
		run: func(map[string]bool) []reduceFinding {
			var fs []reduceFinding
			for _, p := range props {
				if r.unsupported(p, evidence) {
					fs = append(fs, reduceFinding{title: p.Title,
						line: fmt.Sprintf("- %q cites no observation the owner typed, only what a tool or model reported; "+
							"the apply holds it back (learn.require_owner_evidence).", p.Title)})
				}
			}
			return fs
		},
	}, {
		section: "Name collisions",
		warn: func([]reduceFinding) string {
			return "proposed skill name(s) already taken: " + strings.Join(clashes, ", ")
		},
		run: func(map[string]bool) []reduceFinding {
			// A clash names a skill, not a title, so it flags nothing: the
			// budget and the replay never touch a skill anyway.
			var fs []reduceFinding
			for _, name := range clashes {
				fs = append(fs, reduceFinding{
					line: fmt.Sprintf("- `/%s` is already a skill or command; rename this proposal before applying it.", name)})
			}
			return fs
		},
	}, {
		section: "Scope problems",
		warn:    count("%d proposal(s) sit where their evidence says they don't belong"),
		run: func(map[string]bool) []reduceFinding {
			var fs []reduceFinding
			for _, p := range props {
				target := normTarget(p.Target, r.Home)
				if why := r.scopeProblem(p, target, evidence); why != "" {
					fs = append(fs, reduceFinding{title: p.Title,
						line: fmt.Sprintf("- %q: %s; the apply holds it back.", p.Title, why)})
				} else if repo := r.narrowScope(p, target, evidence); repo != nil {
					fs = append(fs, reduceFinding{title: p.Title,
						line: fmt.Sprintf("- %q targets a global file, and every session behind it ran in %s; "+
							"it belongs in that repo's own context file, and the apply holds it back.", p.Title, homeRelative(repo.Root, r.Home))})
				}
			}
			return fs
		},
	}, {
		section: "Off-target proposals",
		warn:    count("%d proposal(s) target files outside learn.targets and learn.repos"),
		run: func(map[string]bool) []reduceFinding {
			var fs []reduceFinding
			for _, o := range OffTargets(out, targets, r.Home) {
				fs = append(fs, reduceFinding{title: o.Title,
					line: fmt.Sprintf("- %q targets %s, which isn't in learn.targets or a learn.repos checkout; move it to a target or drop it.", o.Title, o.Target)})
			}
			return fs
		},
	}, {
		section: "Over budget",
		warn:    count("%d proposal(s) would put a target over its budget"),
		run: func(flagged map[string]bool) []reduceFinding {
			budget := r.budgetCheck()
			var fs []reduceFinding
			for _, p := range props {
				if why := budget(p, normTarget(p.Target, r.Home), flagged[p.Title]); why != "" {
					fs = append(fs, reduceFinding{title: p.Title,
						line: fmt.Sprintf("- %q: %s; the apply holds it back.", p.Title, why)})
				}
			}
			return fs
		},
	}}
}

// NameCollisions returns the names of proposed new skills in a reduce
// output that are already taken, in the order proposed. The model is told
// the taken names; this checks it listened.
func NameCollisions(proposals string, taken []string) []string {
	set := map[string]bool{}
	for _, t := range taken {
		set[strings.ToLower(t)] = true
	}
	var out []string
	inSkill := false
	for _, line := range strings.Split(proposals, "\n") {
		if rest, ok := strings.CutPrefix(line, "### "); ok {
			inSkill = strings.HasPrefix(strings.TrimSpace(rest), "skill")
			continue
		}
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "- name:")
		if !inSkill || !ok {
			continue
		}
		name = strings.TrimPrefix(strings.Trim(strings.TrimSpace(name), "`"), "/")
		if name != "" && set[strings.ToLower(name)] {
			out = append(out, name)
		}
	}
	return out
}

// normTarget is a proposal's target as a path to compare: backticks
// dropped and a leading ~ expanded against home.
func normTarget(p, home string) string {
	p = strings.Trim(strings.TrimSpace(p), "`")
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return filepath.Clean(p)
}

// OffTarget is a proposal whose target isn't a configured target.
type OffTarget struct {
	Title, Target string
}

// OffTargets returns the add, reword, and retire proposals in a reduce
// output whose target isn't one of targets. A ~ in either expands against
// home. Skill and graduate proposals name a skill or a script instead, so
// they aren't checked. With no targets configured, nothing is flagged.
func OffTargets(proposals string, targets []string, home string) []OffTarget {
	if len(targets) == 0 {
		return nil
	}
	norm := func(p string) string { return normTarget(p, home) }
	allowed := map[string]bool{}
	for _, t := range targets {
		allowed[norm(t)] = true
	}
	var out []OffTarget
	title, checked := "", false
	for _, line := range strings.Split(proposals, "\n") {
		if rest, ok := strings.CutPrefix(line, "### "); ok {
			kind, t, _ := strings.Cut(rest, ":")
			kind = strings.TrimSpace(kind)
			checked = kind == "add" || kind == "reword" || kind == "retire"
			title = strings.TrimSpace(t)
			continue
		}
		target, ok := strings.CutPrefix(strings.TrimSpace(line), "- target:")
		if !checked || !ok {
			continue
		}
		checked = false // one target per proposal
		if !allowed[norm(target)] {
			out = append(out, OffTarget{Title: title, Target: strings.TrimSpace(target)})
		}
	}
	return out
}
