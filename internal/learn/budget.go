package learn

import (
	"fmt"
	"os"

	"github.com/micahhausler/recall/internal/review"
)

// Every line of a context file costs context in every session that loads
// it, so a file gets a budget, and a new rule in a full file has to say
// which rule it displaces. Without one, rules only accumulate: the loop
// learns from what went wrong, and nothing ever records that a rule
// stopped being needed.

// tokens estimates how many tokens a text costs: about four characters
// each. It is an estimate to budget by, not a tokenizer.
func tokenCost(text string) int {
	return (len(text) + 3) / 4
}

// fileCost is the estimated token cost of a file; a missing file costs
// nothing.
func fileCost(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return tokenCost(string(data))
}

// budgeter checks a run's adds against the budget in order, so each one
// counts the adds accepted before it: three rules that each fit alone can't
// all land in a file with room for one, and one rule can't be displaced
// twice.
type budgeter struct {
	budget int
	rules  []Rule
	added  map[string]int  // tokens accepted adds put in each target
	gone   map[string]bool // rule ids already displaced
}

func newBudgeter(budget int, rules []Rule) *budgeter {
	return &budgeter{budget: budget, rules: rules, added: map[string]int{}, gone: map[string]bool{}}
}

// check says why a proposal can't go ahead against the budget, if it can't.
// What "- displaces:" names is validated whenever it is set, even when the
// target has room or there is no budget, since the apply removes the
// displaced rule either way: it must be a ledger rule in the proposal's own
// target, and no run displaces one rule twice. An add, or a reword that
// grows its rule, is then counted against the budget with the proposals
// accepted before it, and held back when it is over and displaces nothing.
// A budget of 0 caps nothing.
func (b *budgeter) check(p Proposal, target string) string {
	target = review.Resolve(target)
	freed := 0
	if p.Displaces != "" {
		d := b.rule(p.Displaces)
		switch {
		case d == nil:
			return fmt.Sprintf("it displaces %s, which isn't a rule on the ledger", p.Displaces)
		// The ledger may name the file by a path that links to it.
		case review.Resolve(d.Target) != target:
			return fmt.Sprintf("it displaces %s, which is in %s, not in its target", d.ID, d.Target)
		case b.gone[d.ID]:
			return fmt.Sprintf("it displaces %s, which another proposal in this run already displaces", d.ID)
		}
		freed = tokenCost(d.Text)
	}
	grow := 0
	switch p.Kind {
	case "add":
		grow = tokenCost(p.Rule)
	case "reword":
		// A reword that grows its rule costs the difference; the old
		// wording's tokens are already in the file.
		grow = tokenCost(p.Rule)
		if old := b.rule(p.Replaces); old != nil && review.Resolve(old.Target) == target {
			grow -= tokenCost(old.Text)
		}
	}
	if b.budget > 0 && grow > 0 && p.Displaces == "" {
		if have := fileCost(target) + b.added[target]; have+grow > b.budget {
			what := "this rule adds about"
			if p.Kind == "reword" {
				what = "this reword grows its rule by about"
			}
			return fmt.Sprintf("%s is at about %d of its %d-token budget and %s %d; "+
				"it has to name the rule it displaces", target, have, b.budget, what, grow)
		}
	}
	if p.Displaces != "" {
		b.gone[p.Displaces] = true
	}
	b.added[target] += grow - freed
	return ""
}

// rule returns the ledger rule with that id, or nil.
func (b *budgeter) rule(id string) *Rule {
	if id == "" {
		return nil
	}
	for i := range b.rules {
		if b.rules[i].ID == id {
			return &b.rules[i]
		}
	}
	return nil
}

// overBudget checks one add on its own.
func overBudget(p Proposal, target string, budget int, rules []Rule) string {
	return newBudgeter(budget, rules).check(p, target)
}
