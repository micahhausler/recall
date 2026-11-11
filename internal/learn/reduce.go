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
}

// BuildInput renders the reduce model's input from the window's observations.
func (r *Reducer) BuildInput(window []SessionObservations) string {
	var b strings.Builder
	b.WriteString(reducePrompt)

	b.WriteString("\n\n# CURRENT RULES\n\n")
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
	return b.String()
}

// Run writes proposals.md (the model's output) and input.md (what it saw)
// into outDir, so a review has both.
func (r *Reducer) Run(ctx context.Context, window []SessionObservations, outDir string) (string, error) {
	input := r.BuildInput(window)
	out, err := r.Runner.Run(ctx, r.Model, input)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(outDir, "input.md"), []byte(input), 0o644); err != nil {
		return "", err
	}
	proposals := filepath.Join(outDir, "proposals.md")
	if err := os.WriteFile(proposals, []byte(out), 0o644); err != nil {
		return "", err
	}
	return proposals, nil
}
