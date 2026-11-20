package learn

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/evidence"
	"github.com/micahhausler/recall/internal/store"
)

func TestEvidenceItemsBecomeObservations(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	const repo = "example.com/you/project"
	items := []evidence.Item{
		{Source: evidence.SourceReview, Repo: repo, PR: repo + "#7", Author: "you", Owner: true,
			Text: "use the injected clock here", When: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)},
		{Source: evidence.SourceReview, Repo: repo, PR: repo + "#7", Author: "reviewer",
			Text: "this re-reads the file every call", When: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)},
		{Source: evidence.SourceCI, Repo: repo, PR: repo + "#7",
			Text: "check test failed", When: time.Date(2026, 11, 9, 11, 0, 0, 0, time.UTC)},
		{Source: evidence.SourceReview, Repo: repo, PR: repo + "#8", Author: "reviewer",
			Text: "same bug as last time", When: time.Date(2026, 11, 8, 8, 0, 0, 0, time.UTC)},
	}
	put := func(its []evidence.Item) int {
		t.Helper()
		n, err := PutEvidence(ctx, st, its, "github", now)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := put(items); n != 2 {
		t.Fatalf("stored %d records, want one per pull request", n)
	}
	if n := put(items); n != 0 {
		t.Errorf("unchanged evidence was stored again: %d", n)
	}
	window, err := ListObservations(ctx, st, EvidenceHarness("github"))
	if err != nil || len(window) != 2 {
		t.Fatalf("window = %+v, %v; want 2 records", window, err)
	}
	var seven SessionObservations
	for _, so := range window {
		if so.Session == "pr:"+repo+"#7" {
			seven = so
		}
	}
	if seven.Scope.Key() != repo || seven.Day() != "2026-11-10" || len(seven.Observations) != 3 {
		t.Fatalf("record = %+v", seven)
	}
	for _, o := range seven.Observations {
		switch {
		case strings.HasPrefix(o.Text, "Review by you"):
			// The owner wrote it, so it may satisfy the owner-typed checks.
			if o.Source != SourceOwner || o.Category != "review" {
				t.Errorf("owner comment = %+v", o)
			}
		case strings.HasPrefix(o.Text, "Review by reviewer"):
			if o.Source != SourceReview || o.Category != "review" {
				t.Errorf("review comment = %+v", o)
			}
		case strings.HasPrefix(o.Text, "CI on"):
			if o.Source != SourceCI || o.Category != "ci" {
				t.Errorf("ci failure = %+v", o)
			}
		default:
			t.Errorf("unexpected observation %+v", o)
		}
	}

	// New evidence on the same pull request is a new record, and the
	// newest wins in the window.
	more := append(items, evidence.Item{Source: evidence.SourceReview, Repo: repo, PR: repo + "#7",
		Author: "reviewer", Text: "still broken", When: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)})
	if _, err := PutEvidence(ctx, st, more, "github", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	window, err = ListObservations(ctx, st, EvidenceHarness("github"))
	if err != nil || len(window) != 2 {
		t.Fatalf("window after growth = %d records, %v; want still 2", len(window), err)
	}
	for _, so := range window {
		if so.Session == "pr:"+repo+"#7" && len(so.Observations) != 4 {
			t.Errorf("grown record has %d observations, want 4", len(so.Observations))
		}
	}
}

// Outside evidence is tool-derived: a review comment or CI failure never
// satisfies the owner-typed check, except a comment the owner wrote.
func TestEvidenceHoldback(t *testing.T) {
	cluster := func(src string) []Cluster {
		return []Cluster{{Candidate: Candidate{ID: "c-1"}, Evidence: []Evidence{{
			Session: "pr:example.com/you/project#7", Day: "2026-11-10",
			Observation: Observation{Category: "review", Source: src, Text: "t"},
		}}}}
	}
	add := []Proposal{{Kind: "add", Title: "T", Candidates: []string{"c-1"}}}
	for _, src := range []string{SourceReview, SourceCI} {
		e := runEvidence(cluster(src))
		if e.ownerTyped([]string{"c-1"}) {
			t.Errorf("a %s observation counted as owner-typed", src)
		}
		if held := Unsupported(add, e, 0); len(held) != 1 {
			t.Errorf("an add resting on %s evidence alone wasn't held back", src)
		}
	}
	e := runEvidence(cluster(SourceOwner))
	if !e.ownerTyped([]string{"c-1"}) {
		t.Error("an owner-authored comment didn't count as owner-typed")
	}
	if held := Unsupported(add, e, 0); len(held) != 0 {
		t.Errorf("an add resting on an owner-authored comment was held back: %v", held)
	}
}
