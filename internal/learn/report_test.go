package learn

import (
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/review"
)

func reportSession(session, day string, obs ...Observation) SessionObservations {
	return SessionObservations{Harness: "claude", Host: "laptop-a", Session: session, Prompts: 1,
		LastEvent: day + "T12:00:00Z", Observations: obs}
}

func owned(text, quote string) Observation {
	return Observation{Category: "correction", Text: text, Quote: quote, Source: SourceOwner}
}

func TestBuildReport(t *testing.T) {
	vet := "Run go vet before saying a change is done."
	push := "Ask the owner before any force push."
	window := []SessionObservations{
		reportSession("s1", "2026-11-01", owned(vet, "run go vet first")),
		reportSession("s2", "2026-11-03", owned(vet, "you forgot go vet again")),
		reportSession("s3", "2026-11-05", Observation{Category: "correction", Text: push}),
		reportSession("s4", "2026-11-06", Observation{Category: "correction", Text: push}),
		reportSession("s5", "2026-11-06", owned(push, "never force push without asking")),
		reportSession("s6", "2026-11-07", Observation{Category: "tool-failure", Text: "The linter binary is missing from PATH."}),
		// Outside the window, a note, and outside evidence: none count.
		reportSession("s0", "2026-09-01", owned(vet, "old")),
		{Harness: "claude", Session: "memory:p/notes.md", LastEvent: "2026-11-02T00:00:00Z",
			Observations: []Observation{{Category: CategoryMemory, Text: vet}}},
		{Harness: "evidence-github", Session: "pr-1", LastEvent: "2026-11-02T00:00:00Z",
			Observations: []Observation{{Category: "correction", Text: vet}}},
	}
	cl := &CandidateLedger{}
	r := BuildReport(window, nil, nil, cl, "2026-10-26", "2026-11-08")
	if len(cl.Candidates) != 0 || len(cl.Assigned) != 0 {
		t.Errorf("BuildReport changed the candidate ledger: %+v", cl)
	}
	if r.Sessions != 6 || r.Once != 1 || len(r.Findings) != 2 {
		t.Fatalf("report = %+v", r)
	}
	// Two owner-typed sessions beat three sessions with one.
	vetF, pushF := r.Findings[0], r.Findings[1]
	if vetF.Text != vet || vetF.Sessions != 2 || vetF.Owner != 2 || vetF.First != "2026-11-01" || vetF.Last != "2026-11-03" || vetF.Quote != "you forgot go vet again" {
		t.Errorf("vet finding = %+v", vetF)
	}
	if pushF.Text != push || pushF.Sessions != 3 || pushF.Owner != 1 || pushF.Quote != "never force push without asking" {
		t.Errorf("push finding = %+v", pushF)
	}

	var b strings.Builder
	r.WriteFindings(&b, 1)
	for _, want := range []string{
		"What recall noticed in 6 session(s) since 2026-10-26:",
		"  correction, in 2 sessions, 2026-11-01 to 2026-11-03\n    " + vet + "\n    you said: \"you forgot go vet again\"\n",
		"  1 more repeated lesson(s); recall learn report lists them all",
		"  1 more lesson(s) showed up in one session each",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("findings miss %q:\n%s", want, b.String())
		}
	}
}

// A finding a reduce already cited keeps the candidate's id and says what
// became of the proposal.
func TestBuildReportProposalStatus(t *testing.T) {
	vet := "Run go vet before saying a change is done."
	window := []SessionObservations{
		reportSession("s1", "2026-11-01", owned(vet, "run go vet first")),
		reportSession("s2", "2026-11-03", owned(vet, "run go vet first")),
	}
	cl := &CandidateLedger{}
	cl.Cluster(window, "2026-10-26", "2026-11-04")
	cl.Cite([]Proposal{{Kind: "add", Title: "Run go vet", Candidates: []string{"c-0001"}}}, "2026-11-04")
	for verdict, want := range map[review.Verdict]string{
		review.Pending: `proposed 2026-11-04 as "Run go vet", waiting for review`,
		review.Merged:  `proposed 2026-11-04 as "Run go vet", merged 2026-11-06`,
		review.Closed:  `proposed 2026-11-04 as "Run go vet", closed 2026-11-06`,
	} {
		reviews := []Review{{Proposed: "2026-11-04", Title: "Run go vet", Verdict: verdict, Decided: "2026-11-06"}}
		r := BuildReport(window, nil, &Ledger{Reviews: reviews}, cl, "2026-10-26", "2026-11-08")
		if len(r.Findings) != 1 || r.Findings[0].ID != "c-0001" || r.Findings[0].Status != want {
			t.Errorf("%s: findings = %+v, want status %q", verdict, r.Findings, want)
		}
	}
}

func TestWriteFindingsEmpty(t *testing.T) {
	var b strings.Builder
	Report{Sessions: 3, Once: 2}.WriteFindings(&b, 0)
	if !strings.Contains(b.String(), "What recall noticed in 3 mapped session(s):\n  nothing repeated yet; 2 lesson(s) showed up in one session each") {
		t.Errorf("got %q", b.String())
	}
	b.Reset()
	Report{Cutoff: "2026-10-26"}.WriteFindings(&b, 0)
	if !strings.Contains(b.String(), "nothing yet") {
		t.Errorf("got %q", b.String())
	}
}

func TestShorten(t *testing.T) {
	if got := shorten("  short   quote ", 100); got != "short quote" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("word ", 30)
	if got := shorten(long, 20); got != "word word word word…" {
		t.Errorf("got %q", got)
	}
	if got := shorten(strings.Repeat("é", 20), 5); got != "éé…" {
		t.Errorf("multibyte cut = %q", got)
	}
}

func TestRuleCards(t *testing.T) {
	rules := []Rule{
		{ID: "r-0001", Target: "/home/you/.claude/CLAUDE.md", Text: "Ask before any force push.", TextHash: "h1"},
		{ID: "r-0002", Text: "Keep commit subjects short.", TextHash: "h2"},
		{ID: "r-0003", Text: "Run the tests.", TextHash: "h3", Supersedes: []Version{{TextHash: "h3-old"}}},
		{ID: "r-0004", Text: "Added after every session ran.", TextHash: "h4"},
	}
	all := []RuleRef{{ID: "r-0001", TextHash: "h1"}, {ID: "r-0002", TextHash: "h2"}, {ID: "r-0003", TextHash: "h3"}}
	sh := func(session, day string, hits ...RuleHit) SessionHits {
		return SessionHits{Harness: "claude", Host: "laptop-a", Session: session, LastEvent: day + "T12:00:00Z", Rules: all, Hits: hits}
	}
	judged := []SessionHits{
		sh("s1", "2026-11-01", RuleHit{ID: "r-0001", TextHash: "h1", Status: "followed"}, RuleHit{ID: "r-0003", TextHash: "h3", Status: "followed"}),
		// Judged twice, once followed and once broken: the session counts once, broken.
		sh("s2", "2026-11-02", RuleHit{ID: "r-0001", TextHash: "h1", Status: "followed"}),
		sh("s2", "2026-11-02", RuleHit{ID: "r-0001", TextHash: "h1", Status: "violated"}),
		sh("s3", "2026-11-03", RuleHit{ID: "r-0003", TextHash: "h3", Status: "relevant"}),
		// A hit on a wording the rule replaced, a refused judgment, and one
		// outside the window don't count.
		sh("s4", "2026-11-04", RuleHit{ID: "r-0003", TextHash: "h3-old", Status: "violated"}),
		{Harness: "claude", Session: "s5", LastEvent: "2026-11-05T00:00:00Z", Rules: all, Refused: true},
		sh("s0", "2026-09-01", RuleHit{ID: "r-0002", TextHash: "h2", Status: "violated"}),
	}
	// A judgment stored with the observations still counts.
	observed := []SessionObservations{{Harness: "claude", Host: "laptop-a", Session: "s6", LastEvent: "2026-11-06T00:00:00Z",
		RuleHits: []RuleHit{{ID: "r-0003", TextHash: "h3", Status: "violated"}}}}

	cards := RuleCards(rules, observed, judged, "2026-10-26")
	got := map[string]RuleCard{}
	var order []string
	for _, c := range cards {
		got[c.ID] = c
		order = append(order, c.ID+" "+c.Standing())
	}
	want := "r-0003 broken,r-0001 broken,r-0002 never relevant,r-0004 not judged yet"
	if strings.Join(order, ",") != want {
		t.Errorf("order = %v, want %s", order, want)
	}
	for id, w := range map[string][4]int{
		"r-0001": {4, 2, 1, 1}, // s1 followed, s2 broken, s3 and s4 seen
		"r-0002": {4, 0, 0, 0}, // s1 to s4 judged, none relevant
		"r-0003": {5, 3, 1, 1}, // s1 followed, s3 relevant, s6 broken, s2 and s4 seen
		"r-0004": {0, 0, 0, 0},
	} {
		c := got[id]
		if have := [4]int{c.Judged, c.Relevant, c.Followed, c.Broken}; have != w {
			t.Errorf("%s judged, relevant, followed, broken = %v, want %v", id, have, w)
		}
	}

	var b strings.Builder
	Report{Rules: cards, Home: "/home/you"}.WriteRules(&b)
	for _, want := range []string{
		"Your rules, by what the same sessions show (4):",
		"    r-0001 ~/.claude/CLAUDE.md: Ask before any force push.\n      broken in 1, followed in 1, of 2 relevant session(s)\n",
		"  broken (2):\n    r-0003: Run the tests.\n      broken in 1, followed in 1, of 3 relevant session(s)\n",
		"  never relevant (1):\n    r-0002: Keep commit subjects short.\n      relevant in none of 4 judged session(s)\n",
		"  not judged yet (1):\n",
		"recall learn age lists them",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report card misses %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	Report{}.WriteRules(&b)
	if !strings.Contains(b.String(), "none on the ledger yet") {
		t.Errorf("empty card = %q", b.String())
	}
}

func TestTildeHome(t *testing.T) {
	if got := tildeHome("/home/you/.claude/CLAUDE.md", "/home/you"); got != "~/.claude/CLAUDE.md" {
		t.Errorf("got %q", got)
	}
	if got := tildeHome("/srv/x.md", "/home/you"); got != "/srv/x.md" {
		t.Errorf("got %q", got)
	}
}
