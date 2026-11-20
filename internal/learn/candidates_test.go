package learn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/model"
)

func session(id, day string, texts ...string) SessionObservations {
	so := SessionObservations{Harness: "claude", Session: id, LastEvent: day + "T12:00:00Z"}
	for _, text := range texts {
		so.Observations = append(so.Observations, Observation{Category: "correction", Text: text})
	}
	return so
}

// filler is observations about unrelated things, so the words the test's
// real pair shares are rare in the window, as they are in a real one.
func filler() []SessionObservations {
	topics := []string{
		"Clone Go repos under the import path from go.mod.",
		"Pull request descriptions stay short and high level.",
		"The owner reviews Markdown locally with a terminal viewer.",
		"Backups must never delete data before the upload is confirmed.",
		"Timed practice needs the start time asked for up front.",
		"Home-only jobs belong in the home drop-in, not the shared config.",
		"Explain unfamiliar systems with Kubernetes analogies.",
		"Stage only the intended file when committing.",
	}
	var out []SessionObservations
	for i, text := range topics {
		out = append(out, session(fmt.Sprintf("f%d", i), "2026-10-20", text))
	}
	return out
}

const (
	zshA = "Bash commands with unquoted separator lines failed twice under zsh because zsh expands a leading equals sign."
	zshB = "zsh shell errors hit twice in one session from an unquoted leading equals sign; commands should quote such words."
)

func TestClusterCountsSessions(t *testing.T) {
	window := append(filler(),
		session("s1", "2026-11-01", zshA),
		session("s2", "2026-11-03", zshB),
		session("s2-old", "2026-07-01", zshB), // outside the window
	)
	cl := &CandidateLedger{}
	clusters := cl.Cluster(window, "2026-08-13", "2026-11-11")
	if len(clusters) != len(filler())+1 {
		t.Fatalf("clusters = %d, want the two zsh observations joined and the rest apart", len(clusters))
	}
	open, used := Open(clusters, nil)
	k := open[0]
	if used != 0 || k.Sessions != 2 || k.First != "2026-11-01" || k.Last != "2026-11-03" || k.Text != zshA {
		t.Errorf("most recurring cluster = %+v", k)
	}
	if k.Evidence[0].Session != "s2" {
		t.Errorf("evidence should be newest first: %+v", k.Evidence)
	}

	// The same window clusters the same way again, under the same ids.
	again := cl.Cluster(window, "2026-08-13", "2026-11-12")
	if len(again) != len(clusters) || len(cl.Candidates) != len(clusters) {
		t.Errorf("a second pass made new candidates: %d clusters, %d on the ledger", len(again), len(cl.Candidates))
	}
	path := filepath.Join(t.TempDir(), "state", "candidates.json")
	if err := cl.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadCandidates(path)
	if err != nil || len(back.Candidates) != len(cl.Candidates) || back.Candidates[0].Text != cl.Candidates[0].Text {
		t.Errorf("round trip: %v", err)
	}
	if empty, err := LoadCandidates(filepath.Join(t.TempDir(), "missing.json")); err != nil || len(empty.Candidates) != 0 {
		t.Errorf("missing ledger: %+v, %v", empty, err)
	}
}

// A candidate a proposal used returns to the reduce only with evidence from
// sessions after the owner's verdict.
func TestOpenHoldsBackUsedCandidates(t *testing.T) {
	window := append(filler(), session("s1", "2026-11-01", zshA), session("s2", "2026-11-03", zshB))
	cl := &CandidateLedger{}
	clusters := cl.Cluster(window, "2026-08-13", "2026-11-04")
	open, _ := Open(clusters, nil)
	zsh := open[0].ID
	cl.Cite([]Proposal{{Kind: "add", Title: "Quote for zsh", Candidates: []string{zsh, "c-9999"}}}, "2026-11-04")
	cl.Cite([]Proposal{{Kind: "add", Title: "Quote for zsh", Candidates: []string{zsh}}}, "2026-11-04")
	reclustered := func(w []SessionObservations) []Cluster { return cl.Cluster(w, "2026-08-13", "2026-11-11") }
	find := func(open []Cluster) *Cluster {
		for i := range open {
			if open[i].ID == zsh {
				return &open[i]
			}
		}
		return nil
	}
	if n := len(reclustered(window)[len(filler())].Proposed); n != 1 {
		t.Fatalf("cited twice by one run: %d records", n)
	}

	// The proposal never reached review: the candidate is still open.
	open, used := Open(reclustered(window), nil)
	if k := find(open); used != 0 || k == nil || !strings.Contains(k.Note, "never reached review") {
		t.Errorf("unreviewed: used=%d cluster=%+v", used, k)
	}
	rv := Review{Title: "Quote for zsh", Proposed: "2026-11-04", Verdict: "pending"}
	if open, used = Open(reclustered(window), []Review{rv}); used != 1 || find(open) != nil {
		t.Errorf("a candidate waiting for review was shown again: used=%d", used)
	}
	rv.Verdict, rv.Decided = "closed", "2026-11-05"
	if open, used = Open(reclustered(window), []Review{rv}); used != 1 || find(open) != nil {
		t.Errorf("a rejected candidate with nothing new was shown again: used=%d", used)
	}
	later := append(window, session("s3", "2026-11-09", zshA))
	open, used = Open(reclustered(later), []Review{rv})
	if k := find(open); used != 0 || k == nil || !strings.Contains(k.Note, `rejected 2026-11-05 as "Quote for zsh"; 1 session(s) since`) {
		t.Errorf("new evidence after a rejection: used=%d cluster=%+v", used, k)
	}
	rv.Verdict = "merged"
	open, _ = Open(reclustered(later), []Review{rv})
	if k := find(open); k == nil || !strings.Contains(k.Note, "adopted 2026-11-05") {
		t.Errorf("new evidence after adoption: %+v", k)
	}

	// A candidate with no evidence left is dropped unless a proposal cited it.
	cl.Prune(reclustered(nil))
	if len(cl.Candidates) != 1 || cl.Candidates[0].ID != zsh {
		t.Errorf("after prune = %+v", cl.Candidates)
	}
}

// An observation keeps the candidate it first joined. Without the stored
// assignment, a rejected candidate's evidence drifts to a closer candidate
// created later and comes back as if it were new.
func TestObservationStaysWithItsCandidate(t *testing.T) {
	obs := session("s1", "2026-11-01", zshB)
	ev := Evidence{Harness: obs.Harness, Session: "s1", Observation: obs.Observations[0]}
	cl := &CandidateLedger{
		Candidates: []Candidate{
			{ID: "c-0001", Category: "correction", Text: zshA, Created: "2026-11-01",
				Proposed: []Cited{{Date: "2026-11-04", Kind: "add", Title: "Quote for zsh"}}},
			{ID: "c-0002", Category: "correction", Text: zshB, Created: "2026-11-08"},
		},
		Assigned: map[string]string{ev.key(): "c-0001"},
	}
	window := append(filler(), obs)
	clusters := cl.Cluster(window, "2026-08-13", "2026-11-11")
	for _, k := range clusters {
		switch k.ID {
		case "c-0001":
			if len(k.Evidence) != 1 || k.Evidence[0].Session != "s1" {
				t.Errorf("c-0001 evidence = %+v, want the assigned observation", k.Evidence)
			}
		case "c-0002":
			t.Errorf("the observation drifted to c-0002: %+v", k.Evidence)
		}
	}
	rejected := Review{Title: "Quote for zsh", Proposed: "2026-11-04", Verdict: "closed", Decided: "2026-11-05"}
	open, used := Open(clusters, []Review{rejected})
	for _, k := range open {
		if k.ID == "c-0001" || k.ID == "c-0002" {
			t.Errorf("consumed evidence shown again on %s: %+v", k.ID, k)
		}
	}
	if used != 1 {
		t.Errorf("used = %d, want the rejected candidate held back", used)
	}

	// A fresh observation's assignment is recorded and survives a save; one
	// whose candidate is pruned is re-assigned next time.
	fresh := &CandidateLedger{}
	fresh.Cluster(window, "2026-08-13", "2026-11-11")
	if id := fresh.Assigned[ev.key()]; id == "" {
		t.Errorf("a new observation's candidate wasn't recorded: %+v", fresh.Assigned)
	}
	path := filepath.Join(t.TempDir(), "candidates.json")
	if err := fresh.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadCandidates(path)
	if err != nil || back.Assigned[ev.key()] != fresh.Assigned[ev.key()] {
		t.Errorf("assignment round trip: %v, %+v", err, back.Assigned)
	}
	fresh.Prune(nil)
	if len(fresh.Assigned) != 0 {
		t.Errorf("assignments to pruned candidates survived: %+v", fresh.Assigned)
	}
}

func TestReduceReadsBoundedCandidates(t *testing.T) {
	var window []SessionObservations
	for i := 0; i < maxCandidates+25; i++ {
		// Each observation shares no word with another, so none join.
		window = append(window, session(fmt.Sprintf("s%03d", i), "2026-11-01", fmt.Sprintf("alpha%d beta%d gamma%d", i, i, i)))
	}
	many := session("busy", "2026-11-02")
	for i := 0; i < maxEvidence+3; i++ {
		many.Observations = append(many.Observations, Observation{Category: "context", Text: zshA})
	}
	window = append(window, many)
	fake := &model.Fake{Response: "## Proposals\n\n### add: Quote for zsh\n- target: x\n- rule: Quote it.\n- evidence: c-0001, `c-0002`\n- why: y\n"}
	r := &Reducer{Runner: fake, Ledger: &Ledger{}, Clock: &clock.Fake{T: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}}
	out := filepath.Join(t.TempDir(), "2026-11-11")
	path, _, err := r.Run(context.Background(), window, out)
	if err != nil {
		t.Fatal(err)
	}
	in := fake.Calls[0].Input
	if got := strings.Count(in, "Candidate[c-"); got != maxCandidates {
		t.Errorf("input shows %d candidates, want the cap of %d", got, maxCandidates)
	}
	for _, want := range []string{
		"(26 less recurring candidate(s) not shown)",
		"- (3 more observation(s) not shown)",
	} {
		if !strings.Contains(in, want) {
			t.Errorf("reduce input misses %q", want)
		}
	}
	// The proposal's candidate ids gain the sessions behind them, and the
	// cited candidates are marked as used.
	md, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	props := ParseProposals(string(md))
	if len(props) != 1 || strings.Join(props[0].Candidates, ",") != "c-0001,c-0002" || strings.Join(props[0].Evidence, ",") != "s000,s001" {
		t.Fatalf("proposal evidence = %+v", props)
	}
	cited := 0
	for _, c := range r.Candidates.Candidates {
		if len(c.Proposed) == 1 && c.Proposed[0] == (Cited{Date: "2026-11-11", Kind: "add", Title: "Quote for zsh"}) {
			cited++
		}
	}
	if cited != 2 {
		t.Errorf("cited candidates = %d, want 2", cited)
	}
}
