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
	// Rules is the report card: every rule on the ledger with what the
	// window's rule hits say about it. Home shortens their paths.
	Rules []RuleCard
	Home  string
}

// RuleCard is one rule's record over a report's window, in its current
// wording: how many sessions were judged against it, how many it mattered
// in, and how many of those followed it or broke it.
type RuleCard struct {
	Rule
	Judged, Relevant, Followed, Broken int
}

// Standing sorts a rule into the report card's groups.
func (c RuleCard) Standing() string {
	switch {
	case c.Broken > 0:
		return "broken"
	case c.Relevant > 0:
		return "followed"
	case c.Judged > 0:
		return "never relevant"
	default:
		return "not judged yet"
	}
}

// standings are the report card's groups, in the order it prints them.
var standings = []string{"broken", "followed", "never relevant", "not judged yet"}

// RuleCards scores each rule against the rule hits of the window's
// sessions: a session judged against a rule's current wording counts once,
// at its worst (broken over followed over relevant). A hit on a wording
// the rule has since replaced doesn't count, the way aging reads a rule as
// it reads now.
func RuleCards(rules []Rule, observed []SessionObservations, judged []SessionHits, cutoff string) []RuleCard {
	const (
		seen = iota + 1
		relevant
		followed
		broken
	)
	rank := map[string]int{"relevant": relevant, "followed": followed, "violated": broken}
	worst := map[string]map[string]int{} // rule id -> session -> rank
	mark := func(id, session string, r int) {
		if worst[id] == nil {
			worst[id] = map[string]int{}
		}
		if r > worst[id][session] {
			worst[id][session] = r
		}
	}
	current := map[string]string{}
	for _, r := range rules {
		current[r.ID] = r.TextHash
	}
	wording := func(id, hash string) bool {
		want, ok := current[id]
		return ok && (hash == "" || hash == want)
	}
	for _, sh := range judged {
		if sh.Day() < cutoff || sh.Refused {
			continue
		}
		session := sh.Harness + "\x00" + sh.Host + "\x00" + sh.Session
		for _, ref := range sh.Rules {
			if wording(ref.ID, ref.TextHash) {
				mark(ref.ID, session, seen)
			}
		}
		for _, hit := range sh.Hits {
			if wording(hit.ID, hit.TextHash) {
				mark(hit.ID, session, rank[hit.Status])
			}
		}
	}
	for _, so := range observed { // judgments stored with the observations
		if so.Day() < cutoff {
			continue
		}
		session := so.Harness + "\x00" + so.Host + "\x00" + so.Session
		for _, hit := range so.RuleHits {
			if wording(hit.ID, hit.TextHash) {
				mark(hit.ID, session, rank[hit.Status])
			}
		}
	}
	cards := make([]RuleCard, 0, len(rules))
	for _, r := range rules {
		c := RuleCard{Rule: r}
		for _, rk := range worst[r.ID] {
			c.Judged++
			if rk >= relevant {
				c.Relevant++
			}
			switch rk {
			case followed:
				c.Followed++
			case broken:
				c.Broken++
			}
		}
		cards = append(cards, c)
	}
	sort.SliceStable(cards, func(i, j int) bool {
		a, b := cards[i], cards[j]
		if sa, sb := slices.Index(standings, a.Standing()), slices.Index(standings, b.Standing()); sa != sb {
			return sa < sb
		}
		if a.Broken != b.Broken {
			return a.Broken > b.Broken
		}
		return a.Relevant > b.Relevant
	})
	return cards
}

// maxQuote is how much of a quote a report shows.
const maxQuote = 100

// BuildReport scores the ledger's rules against the window's rule hits,
// and groups the window's observations the way the reduce does,
// on a copy of the candidate ledger, so a finding keeps the id a reduce
// gave it and nothing is saved. Native memory notes, outside evidence, and
// counted repeats aren't the owner's words, so they stay out.
func BuildReport(window []SessionObservations, judged []SessionHits, ledger *Ledger, cl *CandidateLedger, cutoff, today string) Report {
	r := Report{Cutoff: cutoff}
	var reviews []Review
	if ledger != nil {
		reviews = ledger.Reviews
		r.Rules = RuleCards(ledger.Rules, window, judged, cutoff)
	}
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

// Write prints the findings, at most limit of them (0 for all), and then
// the rule report card.
func (r Report) Write(w io.Writer, limit int) {
	r.WriteFindings(w, limit)
	fmt.Fprintln(w)
	r.WriteRules(w)
}

// WriteRules prints the rule report card, one group per standing.
func (r Report) WriteRules(w io.Writer) {
	if len(r.Rules) == 0 {
		fmt.Fprintln(w, "Your rules: none on the ledger yet; recall learn rules sync reads them from learn.targets")
		return
	}
	fmt.Fprintf(w, "Your rules, by what the same sessions show (%d):\n", len(r.Rules))
	for _, standing := range standings {
		var group []RuleCard
		for _, c := range r.Rules {
			if c.Standing() == standing {
				group = append(group, c)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s (%d):\n", standing, len(group))
		for _, c := range group {
			var record string
			switch standing {
			case "broken":
				record = fmt.Sprintf("broken in %d, followed in %d, of %d relevant session(s)", c.Broken, c.Followed, c.Relevant)
			case "followed":
				record = fmt.Sprintf("followed in %d of %d relevant session(s)", c.Followed, c.Relevant)
			case "never relevant":
				record = fmt.Sprintf("relevant in none of %d judged session(s)", c.Judged)
			default:
				record = "no session in the window was judged against it"
			}
			where := ""
			if c.Target != "" {
				where = " " + tildeHome(c.Target, r.Home)
			}
			fmt.Fprintf(w, "    %s%s: %s\n      %s\n", c.ID, where, shorten(c.Text, 90), record)
		}
	}
	if n := countStanding(r.Rules, "never relevant"); n > 0 {
		fmt.Fprintf(w, "  A rule never relevant for learn.retire_days is proposed for retirement; recall learn age lists them.\n")
	}
}

func countStanding(cards []RuleCard, standing string) int {
	n := 0
	for _, c := range cards {
		if c.Standing() == standing {
			n++
		}
	}
	return n
}

// tildeHome writes a path under home with a leading ~.
func tildeHome(path, home string) string {
	if home != "" {
		if rest, ok := strings.CutPrefix(path, home+"/"); ok {
			return "~/" + rest
		}
	}
	return path
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
