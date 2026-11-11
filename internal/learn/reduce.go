package learn

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/model"
)

// Reducer runs the weekly pass: observations + rules + aging in, proposals
// out. Proposals are written for review, never applied to live files.
type Reducer struct {
	Runner model.Runner
	Model  string
	Ledger *Ledger
	// RetireDays is the aging window, default 90.
	RetireDays int
	Clock      clock.Clock
	Out        io.Writer
	// Taken are the skill and slash command names that already exist; a
	// proposed new skill must not reuse one.
	Taken []string
	// Targets are the context files rules may live in (learn.targets);
	// Home expands a ~ in a proposal's target to compare against them.
	Targets []string
	Home    string
}

// BuildInput renders the reduce model's input from the window's observations.
func (r *Reducer) BuildInput(window []SessionObservations) string {
	var b strings.Builder
	b.WriteString(reducePrompt)

	b.WriteString("\n\n# TARGETS\n\n")
	if len(r.Targets) == 0 {
		b.WriteString("(none configured)\n")
	}
	for _, t := range r.Targets {
		fmt.Fprintf(&b, "- %s\n", t)
	}

	b.WriteString("\n# CURRENT RULES\n\n")
	if len(r.Ledger.Rules) == 0 {
		b.WriteString("(none on the ledger yet)\n")
	}
	for _, rule := range r.Ledger.Rules {
		fmt.Fprintf(&b, "Rule[%s] (%s; created %s", rule.ID, rule.Target, rule.Created)
		if rule.LastRelevant != "" {
			fmt.Fprintf(&b, "; last relevant %s", rule.LastRelevant)
		}
		if rule.LastViolated != "" {
			fmt.Fprintf(&b, "; last violated %s", rule.LastViolated)
		}
		fmt.Fprintf(&b, "): %s\n", rule.Text)
	}

	b.WriteString("\n# OBSERVATIONS\n\n")
	total := 0
	for _, so := range window {
		for _, o := range so.Observations {
			if o.Quote != "" {
				fmt.Fprintf(&b, "Quote: %q\n", o.Quote)
			}
			fmt.Fprintf(&b, "Observation[%s] (session %s): %s\n", o.Category, so.Session, o.Text)
			total++
		}
	}
	if total == 0 {
		b.WriteString("(none in the window)\n")
	}

	retireDays := r.RetireDays
	if retireDays == 0 {
		retireDays = 90
	}
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
	input := r.BuildInput(window)
	out, usage, err := r.Runner.Run(ctx, r.Model, input)
	if err != nil {
		return "", usage, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", usage, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "input.md"), []byte(input), 0o644); err != nil {
		return "", usage, err
	}
	if clashes := NameCollisions(out, r.Taken); len(clashes) > 0 {
		var b strings.Builder
		b.WriteString("\n## Name collisions\n\n")
		for _, name := range clashes {
			fmt.Fprintf(&b, "- `/%s` is already a skill or command; rename this proposal before applying it.\n", name)
		}
		out += b.String()
		if r.Out != nil {
			fmt.Fprintf(r.Out, "warning: proposed skill name(s) already taken: %s\n", strings.Join(clashes, ", "))
		}
	}
	if off := OffTargets(out, r.Targets, r.Home); len(off) > 0 {
		var b strings.Builder
		b.WriteString("\n## Off-target proposals\n\n")
		for _, o := range off {
			fmt.Fprintf(&b, "- %q targets %s, which isn't in learn.targets; move it to a target or drop it.\n", o.Title, o.Target)
		}
		out += b.String()
		if r.Out != nil {
			fmt.Fprintf(r.Out, "warning: %d proposal(s) target files outside learn.targets\n", len(off))
		}
	}
	proposals := filepath.Join(outDir, "proposals.md")
	if err := os.WriteFile(proposals, []byte(out), 0o644); err != nil {
		return "", usage, err
	}
	return proposals, usage, nil
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
	norm := func(p string) string {
		p = strings.Trim(strings.TrimSpace(p), "`")
		if p == "~" || strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
		return filepath.Clean(p)
	}
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
