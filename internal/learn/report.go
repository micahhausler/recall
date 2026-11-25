package learn

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/micahhausler/recall/internal/review"
)

// Finding is one lesson the window's sessions repeat, as the owner reads it
// in a report: how often, when, and in the owner's own words.
type Finding struct {
	ID       string
	Category string
	Text     string
	// Sessions is how many distinct sessions showed it, and Owner how many
	// of those carry a quote the binary found in the owner's own prompts.
	Sessions, Owner int
	First, Last     string
	// Quote is the newest owner-typed quote behind it, shortened.
	Quote string
	// Status says what became of a proposal that cited it, when one did.
	Status string
}

// Report is what recall noticed over a window, built from what it already
// stores, with no model call.
type Report struct {
	// Cutoff is the window's first day; empty for every session.
	Cutoff string
	// Sessions is how many mapped sessions the owner typed into fall in
	// the window.
	Sessions int
	// Findings are the lessons two or more sessions showed, most owner
	// sessions first; Once counts the lessons only one session showed.
	Findings []Finding
	Once     int
}

// maxQuote is how much of a quote a report shows.
const maxQuote = 100

// BuildReport groups the window's observations the way the reduce does,
// on a copy of the candidate ledger, so a finding keeps the id a reduce
// gave it and nothing is saved. Native memory notes, outside evidence, and
// counted repeats aren't the owner's words, so they stay out.
func BuildReport(window []SessionObservations, cl *CandidateLedger, reviews []Review, cutoff, today string) Report {
	r := Report{Cutoff: cutoff}
	var sessions []SessionObservations
	for _, so := range window {
		if so.Day() < cutoff || strings.HasPrefix(so.Session, "memory:") || strings.HasPrefix(so.Harness, "evidence-") {
			continue
		}
		if so.Prompts > 0 || len(so.Observations) > 0 {
			r.Sessions++
		}
		sessions = append(sessions, so)
	}
	scratch := cl.clone()
	clusters := scratch.Cluster(sessions, cutoff, today)
	verdict := map[string]Review{}
	for _, rv := range reviews {
		verdict[rv.Proposed+"\x00"+rv.Title] = rv
	}
	for _, k := range clusters {
		if k.Sessions < 2 {
			r.Once++
			continue
		}
		f := Finding{ID: k.ID, Category: k.Category, Text: k.Text, Sessions: k.Sessions, First: k.First, Last: k.Last}
		owners := map[string]bool{}
		for _, ev := range k.Evidence { // newest first
			if ev.Source != SourceOwner {
				continue
			}
			owners[ev.Harness+"\x00"+ev.from()] = true
			if f.Quote == "" {
				f.Quote = shorten(ev.Quote, maxQuote)
			}
		}
		f.Owner = len(owners)
		if n := len(k.Proposed); n > 0 {
			p := k.Proposed[n-1]
			f.Status = fmt.Sprintf("proposed %s as %q", p.Date, p.Title)
			if rv, ok := verdict[p.Date+"\x00"+p.Title]; ok {
				switch rv.Verdict {
				case review.Pending:
					f.Status += ", waiting for review"
				case review.Merged:
					f.Status += ", merged " + rv.Decided
				case review.Closed:
					f.Status += ", closed " + rv.Decided
				}
			}
		}
		r.Findings = append(r.Findings, f)
	}
	sort.SliceStable(r.Findings, func(i, j int) bool {
		a, b := r.Findings[i], r.Findings[j]
		if a.Owner != b.Owner {
			return a.Owner > b.Owner
		}
		if a.Sessions != b.Sessions {
			return a.Sessions > b.Sessions
		}
		return a.Last > b.Last
	})
	return r
}

// WriteFindings prints the report's findings, at most limit of them (0
// for all).
func (r Report) WriteFindings(w io.Writer, limit int) {
	if r.Cutoff != "" {
		fmt.Fprintf(w, "What recall noticed in %d session(s) since %s:\n", r.Sessions, r.Cutoff)
	} else {
		fmt.Fprintf(w, "What recall noticed in %d mapped session(s):\n", r.Sessions)
	}
	if len(r.Findings) == 0 {
		if r.Once > 0 {
			fmt.Fprintf(w, "  nothing repeated yet; %d lesson(s) showed up in one session each\n", r.Once)
		} else {
			fmt.Fprintln(w, "  nothing yet; the map has observed no lesson in these sessions")
		}
		return
	}
	shown := r.Findings
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	for _, f := range shown {
		when := f.Last
		if f.First != f.Last {
			when = f.First + " to " + f.Last
		}
		fmt.Fprintf(w, "  %s, in %d sessions, %s\n", f.Category, f.Sessions, when)
		fmt.Fprintf(w, "    %s\n", f.Text)
		if f.Quote != "" {
			fmt.Fprintf(w, "    you said: %q\n", f.Quote)
		}
		if f.Status != "" {
			fmt.Fprintf(w, "    %s\n", f.Status)
		}
	}
	if n := len(r.Findings) - len(shown); n > 0 {
		fmt.Fprintf(w, "  %d more repeated lesson(s); recall learn report lists them all\n", n)
	}
	if r.Once > 0 {
		fmt.Fprintf(w, "  %d more lesson(s) showed up in one session each\n", r.Once)
	}
}

// clone copies the ledger deeply enough that clustering the copy leaves
// the original as it was.
func (cl *CandidateLedger) clone() *CandidateLedger {
	if cl == nil {
		return &CandidateLedger{}
	}
	return &CandidateLedger{Candidates: slices.Clone(cl.Candidates), Assigned: maps.Clone(cl.Assigned)}
}

// shorten cuts s to at most n bytes at a word boundary, marking the cut.
func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:") + "…"
}
