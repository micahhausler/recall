package learn

import (
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/clock"
)

// A wider reduce window clusters observations older than learn.retire_days,
// while retirement keeps using learn.retire_days.
func TestReduceWindowClustersOldObservationsWithoutMovingRetirement(t *testing.T) {
	clk := &clock.Fake{T: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	old := session("old-session", "2026-05-01", "Use the module proxy for Go downloads.")
	stale := Rule{ID: "r-0001", Target: "base.md", Text: "Prefer table-driven tests.",
		TextHash: textHash("Prefer table-driven tests."), Created: "2026-01-01", LastRelevant: "2026-02-01"}
	build := func(r *Reducer) string {
		r.Ledger = &Ledger{Rules: []Rule{stale}}
		r.Clock = clk
		r.RetireDays = 90
		r.Candidates = &CandidateLedger{}
		return r.BuildInput([]SessionObservations{old})
	}

	in := build(&Reducer{})
	if strings.Contains(in, "module proxy") {
		t.Error("the default window read an observation older than learn.retire_days")
	}
	if !strings.Contains(in, "retire Rule[r-0001] (no relevance in 90 days)") {
		t.Errorf("the stale rule isn't proposed for retirement:\n%s", in)
	}

	for name, r := range map[string]*Reducer{
		"--window all":     {AllWindow: true},
		"window_days: 365": {WindowDays: 365},
	} {
		in := build(r)
		if !strings.Contains(in, "module proxy") {
			t.Errorf("%s: the old observation isn't a candidate:\n%s", name, in)
		}
		if !strings.Contains(in, "retire Rule[r-0001] (no relevance in 90 days)") {
			t.Errorf("%s: retirement moved off learn.retire_days:\n%s", name, in)
		}
	}
}
