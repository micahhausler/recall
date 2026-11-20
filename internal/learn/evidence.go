package learn

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/evidence"
	"github.com/micahhausler/recall/internal/redact"
	"github.com/micahhausler/recall/internal/store"
)

// SourceReview marks an observation read from a review comment on an
// agent-authored change, and SourceCI one from a failed check. Both are
// evidence from outside the chat: other people's words and machine output,
// tool-derived, never owner-typed. The one exception is a comment the owner
// wrote, which carries SourceOwner instead, so it satisfies the owner-typed
// checks the way a verified quote does.
const (
	SourceReview = evidence.SourceReview
	SourceCI     = evidence.SourceCI
)

// EvidenceHarness is the store prefix one evidence source's records live
// under, beside the harnesses' own, so ListObservations reads them the
// same way.
func EvidenceHarness(source string) string { return "evidence-" + source }

// evidenceBudget is how much of one comment or failure an observation keeps.
const evidenceBudget = 300

// evidenceText is one line for an item, through the redactor: the text is
// other people's and could quote anything.
func evidenceText(it evidence.Item) string {
	text := strings.Join(strings.Fields(it.Text), " ")
	if text == "" {
		return ""
	}
	switch {
	case it.Source == evidence.SourceReview && it.Author != "":
		text = fmt.Sprintf("Review by %s on %s: %s", it.Author, it.PR, text)
	case it.Source == evidence.SourceReview:
		text = fmt.Sprintf("Review on %s: %s", it.PR, text)
	default:
		text = fmt.Sprintf("CI on %s: %s", it.PR, text)
	}
	text, _ = redact.String(text)
	if r := []rune(text); len(r) > evidenceBudget {
		text = string(r[:evidenceBudget]) + "…"
	}
	return text
}

// EvidenceRecords groups one source's items into observation records, one
// per pull request, dated by the items' own times. A pull request stands in
// for a session: its comments and failures are one record, and its repo
// scopes them the way a checkout's origin scopes a session.
func EvidenceRecords(items []evidence.Item, source string, now time.Time) []SessionObservations {
	byPR := map[string][]evidence.Item{}
	var order []string
	for _, it := range items {
		if _, ok := byPR[it.PR]; !ok {
			order = append(order, it.PR)
		}
		byPR[it.PR] = append(byPR[it.PR], it)
	}
	var out []SessionObservations
	for _, pr := range order {
		its := byPR[pr]
		so := SessionObservations{
			Harness:    EvidenceHarness(source),
			Session:    "pr:" + pr,
			ObservedAt: now.UTC().Format(time.RFC3339),
			Scope:      Scope{Remote: its[0].Repo},
		}
		var first, last time.Time
		parts := []string{pr, its[0].Repo}
		for _, it := range its {
			text := evidenceText(it)
			if text == "" {
				continue
			}
			src := it.Source
			if it.Owner && it.Source == evidence.SourceReview {
				src = SourceOwner
			}
			so.Observations = append(so.Observations, Observation{Category: it.Source, Source: src, Text: text})
			parts = append(parts, src, text)
			if !it.When.IsZero() {
				if first.IsZero() || it.When.Before(first) {
					first = it.When
				}
				if it.When.After(last) {
					last = it.When
				}
			}
		}
		if len(so.Observations) == 0 {
			continue
		}
		if !first.IsZero() {
			so.FirstEvent = first.UTC().Format(time.RFC3339)
			so.LastEvent = last.UTC().Format(time.RFC3339)
		}
		so.Fingerprint = shortHash(parts...)
		out = append(out, so)
	}
	return out
}

// PutEvidence stores a record per pull request whose evidence isn't stored
// at its current content, and returns how many were new or changed.
func PutEvidence(ctx context.Context, st store.Store, items []evidence.Item, source string, now time.Time) (added int, err error) {
	for _, so := range EvidenceRecords(items, source, now) {
		key := fmt.Sprintf("analysis/%s/%s-%s.json", so.Harness, shortHash(so.Session), so.Fingerprint)
		if _, err := st.Head(ctx, key); err == nil {
			continue
		}
		if err := putJSON(ctx, st, key, so); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}
