package learn

import (
	"fmt"

	"github.com/micahhausler/recall/internal/review"
)

// Gates are the checks the reduce and the apply share, and the config they
// read. The Reducer flags a proposal with them and the Applier holds it back
// with them; both embed one Gates, built once in cmd from the same config,
// so the two steps provably run the same checks.
type Gates struct {
	// Targets are the global context files rules may live in
	// (learn.targets), and Repos the checkouts whose own context files a
	// repo-only lesson may land in (learn.repos). Home expands a ~ in a
	// proposal's target to compare against them.
	Targets []string
	Repos   []Repo
	Home    string
	// Skills and Bin are where a new skill and a graduated script go
	// (learn.skills, learn.bin). Settings is the harness settings file a
	// graduated hook or permission rule goes in (learn.settings); empty
	// leaves those unoffered.
	Skills, Bin, Settings string
	// Taken are the skill and slash command names that already exist; a
	// proposed new skill must not reuse one.
	Taken []string
	// Budget is how many tokens a target may cost (learn.budget); 0 is no
	// budget.
	Budget int
	// Ledger is the one source of the rules the budget and scope checks
	// read, and of the reviews — earlier proposals and the owner's verdicts
	// — that hold a matching proposal back. Nil means no rules and no
	// reviews.
	Ledger *Ledger
	// RequireOwnerEvidence holds back a new rule or skill that cites no
	// observation the owner typed (learn.require_owner_evidence).
	// RepeatEvidenceSessions, when above 0, lets an add rest on repeat
	// evidence alone when the repeat was counted in at least that many
	// sessions (learn.repeat_evidence_sessions).
	RequireOwnerEvidence   bool
	RepeatEvidenceSessions int
}

// rules are the ledger's rules, which the budget and scope checks read.
func (g *Gates) rules() []Rule {
	if g.Ledger == nil {
		return nil
	}
	return g.Ledger.Rules
}

// reviews are the ledger's reviews: earlier proposals and the owner's
// verdicts.
func (g *Gates) reviews() []Review {
	if g.Ledger == nil {
		return nil
	}
	return g.Ledger.Reviews
}

// reviewed says why p is held back for an earlier review, if it is: the
// same proposal still waits for the owner, or the same text was rejected.
func (g *Gates) reviewed(p Proposal) string {
	h := ruleHash(p.Rule)
	for _, rv := range g.reviews() {
		switch rv.Verdict {
		case review.Pending:
			if (rv.Kind == p.Kind && rv.Title == p.Title) || (h != "" && rv.RuleHash == h) {
				return "already waiting for review in " + rv.Place()
			}
		case review.Closed:
			if h != "" && rv.RuleHash == h {
				return fmt.Sprintf("the same text was rejected in %s on %s", rv.Place(), rv.Decided)
			}
		}
	}
	return ""
}

// budgetCheck returns one run's budget check, run over the run's proposals
// in order; it returns why a proposal can't go ahead against the budget, or
// "". The parity rule the reduce and the apply must share lives here and
// nowhere else: the budget counts only proposals that no earlier check held
// back (held) and that no earlier review holds back, matching apply.Plan,
// which skips both before the budget so a skipped proposal frees its room
// for the adds after it.
func (g *Gates) budgetCheck() func(p Proposal, target string, held bool) string {
	b := newBudgeter(g.Budget, g.rules())
	return func(p Proposal, target string, held bool) string {
		if held || g.reviewed(p) != "" {
			return ""
		}
		return b.check(p, target)
	}
}

// scopeProblem says why a proposal can't go to its repo-scoped place, if it
// can't, by the reduce run's evidence record.
func (g *Gates) scopeProblem(p Proposal, target string, e RunEvidence) string {
	return scopeProblem(p, target, g.Repos, e, g.Home, g.rules())
}

// narrowScope names the one listed repo all of a global add's evidence
// comes from, if there is one: the rule belongs in that repo's own context
// file.
func (g *Gates) narrowScope(p Proposal, target string, e RunEvidence) *Repo {
	return narrowScope(p, target, g.Repos, e, g.Home)
}

// unsupported reports whether the gates hold p back for citing no
// observation the owner typed.
func (g *Gates) unsupported(p Proposal, e RunEvidence) bool {
	return g.RequireOwnerEvidence && needsOwnerEvidence(p) && !supported(p, e, g.RepeatEvidenceSessions)
}
