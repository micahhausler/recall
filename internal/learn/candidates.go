package learn

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/micahhausler/recall/internal/review"
)

// Candidate is one lesson the sessions keep showing: the anchor of a group
// of alike observations. The binary, not a model, decides which
// observations belong to it and counts the sessions behind it, so the
// reduce reads a bounded list of counted clusters rather than every
// observation ever mapped.
type Candidate struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	// Text is the wording of the first observation in the group; later
	// observations join by how alike their words are to it.
	Text    string `json:"text"`
	Created string `json:"created"`
	// Proposed are the reduce proposals that cited the candidate. One that
	// went to review marks the candidate as used: it returns to the reduce
	// only with evidence from sessions after the owner's verdict.
	Proposed []Cited `json:"proposed,omitempty"`
}

// Cited is one proposal that cited a candidate.
type Cited struct {
	Date  string `json:"date"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// CandidateLedger is every candidate the reduce has seen, kept beside the
// reduce runs on the machine that runs them.
type CandidateLedger struct {
	Candidates []Candidate `json:"candidates"`
	// Assigned maps each observation, by its key, to the candidate it
	// joined. Membership is decided once: a later run keeps the assignment,
	// so evidence behind a candidate that was proposed or rejected never
	// drifts to a closer candidate created since and comes back as new.
	Assigned map[string]string `json:"assigned,omitempty"`
}

// Evidence is one observation in a cluster, with the session it came from.
type Evidence struct {
	Harness string
	Host    string
	Session string
	// Day is the day the session last ran, and Scope where.
	Day   string
	Scope Scope
	// Models are the models that answered in the session.
	Models []string
	Observation
}

// Cluster is a candidate with the evidence the window holds for it.
type Cluster struct {
	Candidate
	// Evidence is newest first.
	Evidence []Evidence
	// Sessions is how many distinct sessions the evidence comes from, and
	// First and Last the days they span.
	Sessions    int
	First, Last string
	// Repos are the repos the evidence's sessions ran in, by Scope.Key,
	// sorted, and Placed how many of the sessions have one. A session
	// outside any checkout, or mapped before scope was recorded, has none.
	Repos  []string
	Placed int
	// Note says why a candidate a proposal already used is shown again.
	Note string
}

// joinSimilarity is how alike an observation's words must be to a
// candidate's, weighted by rarity, to join it. It is set for precision:
// among one owner's 129 real observations, every cross-session pair at or
// above it was the same lesson, and unrelated pairs began just below. The
// map model words the same lesson differently each time, so many true
// repeats score lower and stay separate candidates. That makes a
// candidate's session count a floor, and the reduce may cite several
// candidates for one lesson.
const joinSimilarity = 0.17

// LoadCandidates reads a candidate ledger; a missing file is an empty one.
func LoadCandidates(path string) (*CandidateLedger, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &CandidateLedger{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cl CandidateLedger
	if err := json.Unmarshal(data, &cl); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cl, nil
}

// Save writes the candidate ledger atomically.
func (cl *CandidateLedger) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cl, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (cl *CandidateLedger) nextID() int {
	max := 0
	for _, c := range cl.Candidates {
		var n int
		if _, err := fmt.Sscanf(c.ID, "c-%d", &n); err == nil && n > max {
			max = n
		}
	}
	return max
}

// Cluster groups the window's observations under candidates. Only
// observations from sessions that last ran on or after cutoff count. An
// observation the ledger has seen keeps the candidate it joined; a new one
// joins the candidate whose words are most like its own, when they are
// alike enough, and otherwise starts a new candidate. New observations are
// taken oldest session first, so the same window always yields the same
// clusters. A candidate with no evidence in the window is left out of the
// result but stays on the ledger.
func (cl *CandidateLedger) Cluster(window []SessionObservations, cutoff, today string) []Cluster {
	var all []Evidence
	for _, so := range window {
		day := so.Day()
		if day < cutoff {
			continue
		}
		for _, o := range so.Observations {
			all = append(all, Evidence{Harness: so.Harness, Host: so.Host, Session: so.Session, Day: day, Scope: so.Scope, Models: so.Models, Observation: o})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Day != all[j].Day {
			return all[i].Day < all[j].Day
		}
		return all[i].Session < all[j].Session
	})

	evWords := make([]map[string]bool, len(all))
	for i, ev := range all {
		evWords[i] = tokens(ev.Text)
	}
	rare := newRarity(evWords)
	words := make([]map[string]bool, len(cl.Candidates))
	at := map[string]int{}
	for i, c := range cl.Candidates {
		words[i] = tokens(c.Text)
		at[c.ID] = i
	}
	if cl.Assigned == nil {
		cl.Assigned = map[string]string{}
	}
	members := map[int][]Evidence{}
	next := cl.nextID()
	for n, ev := range all {
		// An observation already assigned stays with its candidate, even
		// when a closer one has appeared since: a proposed or rejected
		// candidate's evidence is consumed, and never supports another.
		if i, ok := at[cl.Assigned[ev.key()]]; ok {
			members[i] = append(members[i], ev)
			continue
		}
		best, bestSim := -1, 0.0
		for i := range cl.Candidates {
			if sim := rare.similarity(words[i], evWords[n]); sim >= joinSimilarity && sim > bestSim {
				best, bestSim = i, sim
			}
		}
		if best < 0 {
			next++
			cl.Candidates = append(cl.Candidates, Candidate{
				ID: fmt.Sprintf("c-%04d", next), Category: ev.Category, Text: ev.Text, Created: today,
			})
			words = append(words, evWords[n])
			best = len(cl.Candidates) - 1
			at[cl.Candidates[best].ID] = best
		}
		cl.Assigned[ev.key()] = cl.Candidates[best].ID
		members[best] = append(members[best], ev)
	}

	var out []Cluster
	for i, c := range cl.Candidates {
		evs := members[i]
		if len(evs) == 0 {
			continue
		}
		k := Cluster{Candidate: c, First: evs[0].Day, Last: evs[len(evs)-1].Day}
		sessions, repos := map[string]bool{}, map[string]bool{}
		for j := len(evs) - 1; j >= 0; j-- { // newest first
			k.Evidence = append(k.Evidence, evs[j])
			id := evs[j].Harness + "\x00" + evs[j].from()
			key := evs[j].Scope.Key()
			if key != "" && !sessions[id] {
				k.Placed++
			}
			sessions[id] = true
			if key != "" && !repos[key] {
				repos[key] = true
				k.Repos = append(k.Repos, key)
			}
		}
		sort.Strings(k.Repos)
		k.Sessions = len(sessions)
		out = append(out, k)
	}
	return out
}

// key identifies one observation across runs: the session it counts for
// and the observation's own words, so the ledger can keep it with the
// candidate it first joined.
func (ev Evidence) key() string {
	return ev.Harness + "\x00" + ev.from() + "\x00" + shortHash(ev.Category, ev.Text)
}

// from is the session an observation counts for: the one it was read from,
// or for a native memory note, the session the note says it came from. A
// note that names no session counts for its own words: a dotfiles-synced
// note is read on every machine, each under that machine's project key, so
// the key would count one logical note once per host toward every session
// threshold (learn.repeat_evidence_sessions among them). Identical copies
// share their text, and distinct notes don't, so content is the identity.
func (ev Evidence) from() string {
	if ev.Origin != "" {
		return ev.Origin
	}
	if strings.HasPrefix(ev.Session, "memory:") {
		return "memory:" + shortHash(ev.Text)
	}
	return ev.Session
}

// sessionsSince counts the distinct sessions in a cluster that ran after day.
func (k Cluster) sessionsSince(day string) int {
	sessions := map[string]bool{}
	for _, ev := range k.Evidence {
		if ev.Day > day {
			sessions[ev.Harness+"\x00"+ev.from()] = true
		}
	}
	return len(sessions)
}

// Open returns the clusters the reduce should see, most recurring first,
// and how many were held back as already used. A candidate whose proposal
// is waiting for the owner's review is held back. One the owner merged or
// rejected returns only with evidence from sessions after that verdict,
// with a note saying so. A proposal that never reached review doesn't use
// the candidate up.
func Open(clusters []Cluster, reviews []Review) (open []Cluster, used int) {
	verdict := map[string]Review{}
	for _, rv := range reviews {
		verdict[rv.Proposed+"\x00"+rv.Title] = rv
	}
	for _, k := range clusters {
		show := true
		// The newest proposal that reached review decides.
		for i := len(k.Proposed) - 1; i >= 0; i-- {
			p := k.Proposed[i]
			rv, ok := verdict[p.Date+"\x00"+p.Title]
			if !ok {
				k.Note = fmt.Sprintf("proposed %s as %q, which never reached review", p.Date, p.Title)
				continue
			}
			switch rv.Verdict {
			case review.Pending:
				show = false
			case review.Merged, review.Closed:
				word := "adopted"
				if rv.Verdict == review.Closed {
					word = "rejected"
				}
				if n := k.sessionsSince(rv.Decided); n > 0 {
					k.Note = fmt.Sprintf("%s %s as %q; %d session(s) since", word, rv.Decided, p.Title, n)
				} else {
					show = false
				}
			}
			break
		}
		if show {
			open = append(open, k)
		} else {
			used++
		}
	}
	sort.SliceStable(open, func(i, j int) bool {
		if open[i].Sessions != open[j].Sessions {
			return open[i].Sessions > open[j].Sessions
		}
		return open[i].Last > open[j].Last
	})
	return open, used
}

var candidateIDRE = regexp.MustCompile(`\bc-\d+\b`)

// Cite records on each candidate the proposals that cited it, by the
// candidate ids in their evidence. Recording the same run twice adds
// nothing.
func (cl *CandidateLedger) Cite(proposals []Proposal, date string) {
	byID := map[string]*Candidate{}
	for i := range cl.Candidates {
		byID[cl.Candidates[i].ID] = &cl.Candidates[i]
	}
	for _, p := range proposals {
		for _, id := range p.Candidates {
			c, ok := byID[id]
			if !ok {
				continue
			}
			cited := Cited{Date: date, Kind: p.Kind, Title: p.Title}
			dup := false
			for _, have := range c.Proposed {
				dup = dup || have == cited
			}
			if !dup {
				c.Proposed = append(c.Proposed, cited)
			}
		}
	}
}

// Prune drops candidates that have no evidence in the window and that no
// proposal ever cited: their anchors hold nothing a later run needs. An
// assignment to a dropped candidate goes with it, so an observation that
// somehow returns is grouped afresh.
func (cl *CandidateLedger) Prune(clusters []Cluster) {
	live := map[string]bool{}
	for _, k := range clusters {
		live[k.ID] = true
	}
	kept := cl.Candidates[:0]
	keep := map[string]bool{}
	for _, c := range cl.Candidates {
		if live[c.ID] || len(c.Proposed) > 0 {
			kept = append(kept, c)
			keep[c.ID] = true
		}
	}
	cl.Candidates = kept
	for key, id := range cl.Assigned {
		if !keep[id] {
			delete(cl.Assigned, key)
		}
	}
}

// The reduce reads at most maxCandidates clusters, and at most
// maxEvidence observations of each; the rest are counted, not shown. At a
// line or two each, that keeps the candidates under roughly 25,000 tokens
// however many sessions the window holds.
const (
	maxCandidates = 200
	maxEvidence   = 4
)

// renderCandidates writes the CANDIDATES section of the reduce input.
func renderCandidates(b *strings.Builder, open []Cluster, used int) {
	if len(open) == 0 {
		b.WriteString("(none in the window)\n")
	}
	shown := open
	if len(shown) > maxCandidates {
		shown = shown[:maxCandidates]
	}
	for _, k := range shown {
		span := k.Last
		if k.First != k.Last {
			span = k.First + " to " + k.Last
		}
		fmt.Fprintf(b, "Candidate[%s] (%d session(s); %s", k.ID, k.Sessions, span)
		switch {
		case len(k.Repos) == 0:
		case len(k.Repos) == 1 && k.Placed == k.Sessions:
			fmt.Fprintf(b, "; repo %s", k.Repos[0])
		case len(k.Repos) == 1:
			// Never claim a repo for sessions whose place isn't known.
			fmt.Fprintf(b, "; %d of them in repo %s, the rest in no known repo", k.Placed, k.Repos[0])
		default:
			fmt.Fprintf(b, "; %d repos", len(k.Repos))
		}
		if k.Note != "" {
			fmt.Fprintf(b, "; %s", k.Note)
		}
		b.WriteString(")\n")
		for i, ev := range k.Evidence {
			if i == maxEvidence {
				fmt.Fprintf(b, "- (%d more observation(s) not shown)\n", len(k.Evidence)-maxEvidence)
				break
			}
			label := ev.Category
			switch ev.Source {
			case SourceOwner:
				label += ", owner-typed"
			case SourceRepeat:
				label += ", tool-derived"
			}
			fmt.Fprintf(b, "- [%s] %s %s", label, ev.Day, ev.place())
			if (len(k.Repos) > 1 || k.Placed < k.Sessions) && ev.Scope.Key() != "" {
				fmt.Fprintf(b, " in %s", ev.Scope.Key())
			}
			fmt.Fprintf(b, ": %s", ev.Text)
			switch {
			case ev.Quote != "" && ev.Source == SourceOwner:
				fmt.Fprintf(b, " Owner: %q", ev.Quote)
			case ev.Quote != "":
				// From before quotes were checked against the session.
				fmt.Fprintf(b, " Unchecked quote: %q", ev.Quote)
			}
			b.WriteString("\n")
		}
	}
	if n := len(open) - len(shown); n > 0 {
		fmt.Fprintf(b, "\n(%d less recurring candidate(s) not shown)\n", n)
	}
	if used > 0 {
		fmt.Fprintf(b, "\n(%d candidate(s) held back: already proposed, with no new evidence since the owner's verdict)\n", used)
	}
}

// place says where an observation came from: a session, or a native
// memory note, with the session that wrote it when the note says.
func (ev Evidence) place() string {
	if note, ok := strings.CutPrefix(ev.Session, "memory:"); ok {
		if ev.Origin != "" {
			return fmt.Sprintf("note %s from session %s", note, shortSession(ev.Origin))
		}
		return "note " + note
	}
	return "session " + shortSession(ev.Session)
}

// shortSession is enough of a session id to tell sessions apart in a list.
func shortSession(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ExpandEvidence rewrites each proposal's evidence line so every candidate
// id it cites is followed by the sessions behind that candidate. The
// reduce cites candidates; a reviewer, and the ledger, want sessions. A
// candidate whose evidence is only repeats the binary counted is labeled
// tool-derived, in the proposals and so in the pull request that carries
// them.
func ExpandEvidence(proposals string, clusters []Cluster) string {
	sessions := map[string][]string{}
	repeats := map[string]bool{}
	for _, k := range clusters {
		seen := map[string]bool{}
		only := len(k.Evidence) > 0
		for _, ev := range k.Evidence {
			only = only && ev.Source == SourceRepeat
			if !seen[ev.from()] {
				seen[ev.from()] = true
				sessions[k.ID] = append(sessions[k.ID], ev.from())
			}
		}
		repeats[k.ID] = only
	}
	lines := strings.Split(proposals, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "- evidence:") {
			continue
		}
		lines[i] = candidateIDRE.ReplaceAllStringFunc(line, func(id string) string {
			if s := sessions[id]; len(s) > 0 {
				note := "sessions "
				if repeats[id] {
					note = "tool-derived repeats; sessions "
				}
				return id + " (" + note + strings.Join(s, ", ") + ")"
			}
			return id
		})
	}
	return strings.Join(lines, "\n")
}
