package learn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/review"
)

// Rule is one standing rule on the ledger. The ledger is a separate file
// kept beside the context files, never comments inside them, because
// comments cost context in every session.
//
// A rule's id is stable: rewording the bullet keeps the id, the dates, and
// the violation history, and records the text it replaced under Supersedes.
type Rule struct {
	ID             string   `json:"id"`
	Target         string   `json:"target"`
	Text           string   `json:"text"`
	TextHash       string   `json:"text_hash"`
	SourceSessions []string `json:"source_sessions,omitempty"`
	Created        string   `json:"created"`
	// Since is the day the current text took effect: the day a new bullet
	// appeared, or the day of its last reword. Empty means the text was
	// already in the file when the ledger first read it.
	Since        string `json:"since,omitempty"`
	LastRelevant string `json:"last_relevant,omitempty"`
	LastViolated string `json:"last_violated,omitempty"`
	// Supersedes are the rule's earlier wordings, oldest first.
	Supersedes []Version `json:"supersedes,omitempty"`
	// Models are the models that answered in the sessions that earned the
	// rule's current wording. A rule exists because a model needed it; one
	// none of today's sessions run on may be a rule nothing needs.
	Models []string `json:"models,omitempty"`
}

// Version is one earlier wording of a rule and the day it was replaced.
type Version struct {
	TextHash string `json:"text_hash"`
	Text     string `json:"text"`
	Until    string `json:"until"`
}

// Wrote reports whether hash is the rule's text now or at any earlier time.
func (r Rule) Wrote(hash string) bool {
	if hash == r.TextHash {
		return true
	}
	for _, v := range r.Supersedes {
		if v.TextHash == hash {
			return true
		}
	}
	return false
}

// Ledger is every rule recall tracks, and every proposal an apply sent for
// review with the owner's verdict on it.
type Ledger struct {
	Rules   []Rule   `json:"rules"`
	Reviews []Review `json:"reviews,omitempty"`
	// LastRule is the highest rule number ever given out, so the id of a
	// deleted rule is never given to a new one: an old proposal's
	// "- replaces:" must not land on a rule it never meant.
	LastRule int `json:"last_rule,omitempty"`
	// Folded maps a per-host review object (PutHostReviews) to the review
	// keys it carried, recorded when the reducer folds the object in. An
	// object's store key hashes its batch, which can't be checked against
	// the ledger from the key alone, so the ledger carries the set of
	// folded objects instead, and a later sync skips reading them. Once the
	// published store ledger holds everything an object carried, the
	// reducer deletes the object and drops its entry, so neither the store
	// nor this map keeps settled batches.
	Folded map[string][]string `json:"folded,omitempty"`
}

// Review is one proposal an apply sent for review. A closed one is the
// owner saying no, which the reduce reads so it doesn't ask again.
type Review struct {
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Rule     string `json:"rule"`
	RuleHash string `json:"rule_hash"`
	// Replaces is the id of the rule a reword, retire, or graduate changes;
	// Evidence is the sessions the proposal cited.
	Replaces string   `json:"replaces,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
	// Models are the models that answered in the cited sessions.
	Models []string `json:"models,omitempty"`
	// Proposed is the reduce run's date.
	Proposed string `json:"proposed"`
	// Via is the publisher (learn.review) and Where what it returned, like
	// a pull request URL; the rest find a branch's fate in its repo.
	Via    string `json:"via"`
	Where  string `json:"where"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	Commit string `json:"commit,omitempty"`
	// Verdict is pending, merged, or closed; Decided is the day it changed
	// from pending.
	Verdict review.Verdict `json:"verdict"`
	Decided string         `json:"decided,omitempty"`
}

// Place says where the review is for a person: a pull request URL, or a
// branch and the repo it's in.
func (rv Review) Place() string {
	// A pull request URL says where by itself.
	if strings.Contains(rv.Where, "://") || rv.Repo == "" {
		return rv.Where
	}
	return rv.Where + " in " + rv.Repo
}

// reviewKey identifies one published proposal: the key RecordReviews and
// SyncLedger dedupe reviews by.
func reviewKey(rv Review) string {
	return rv.Repo + "\x00" + rv.Where + "\x00" + rv.Kind + "\x00" + rv.Title
}

// RecordReviews adds a pending review for each proposal a finished apply
// published. Each review is linked to its proposal by the plan's route id,
// carried through the apply, never by the title the model echoed, and its
// rule is the text that landed in the staged diff when the apply read one.
// Recording the same run twice adds nothing.
func (l *Ledger) RecordReviews(plan *Plan, res *ApplyResult, via string) int {
	seen := map[string]bool{}
	for _, rv := range l.Reviews {
		seen[reviewKey(rv)] = true
	}
	added := 0
	record := func(rv Review) {
		if seen[reviewKey(rv)] {
			return
		}
		l.Reviews = append(l.Reviews, rv)
		seen[reviewKey(rv)] = true
		added++
	}
	for _, c := range res.Changes {
		if c.Error != "" || c.Where == "" {
			continue // not in front of the owner
		}
		for i, title := range c.Proposals {
			rv := Review{Title: title, Proposed: plan.Date, Via: via, Where: c.Where, Repo: c.Repo,
				Branch: c.Branch, Base: c.Base, Commit: c.Commit, Verdict: review.Pending}
			var route *Route
			if i < len(c.Routes) {
				route = plan.route(c.Routes[i])
			}
			if route == nil {
				// An outcome the binary couldn't link to a route; keep what
				// the model said, with nothing to mislink.
				record(rv)
				continue
			}
			rv.Kind, rv.Rule = route.Kind, route.Rule
			if landed := c.Landed[route.ID]; landed != "" {
				rv.Rule = landed
			}
			// The proposal's own commit, so the owner's verdict on it can be
			// read apart from the rest of the change.
			if commit := c.Commits[route.ID]; commit != "" {
				rv.Commit = commit
			}
			rv.RuleHash = ruleHash(rv.Rule)
			rv.Replaces, rv.Evidence, rv.Models = route.Replaces, route.Evidence, route.Models
			record(rv)
			// A proposal that displaces a rule retires it in the same
			// change, so the displaced rule gets the same ledger history a
			// retire gets, under this proposal's review and verdict.
			if route.Displaces != "" {
				for _, r := range l.Rules {
					if r.ID == route.Displaces {
						drv := rv
						drv.Kind, drv.Rule, drv.RuleHash = "retire", r.Text, ruleHash(r.Text)
						drv.Replaces = r.ID
						record(drv)
					}
				}
			}
		}
	}
	return added
}

// RefreshReviews asks each pending review's publisher what became of it,
// once per place a change landed. The apply stages one commit per proposal,
// so each review carries its own commit and gets its own verdict; after a
// merge, a proposal whose rule text did not land in the base is recorded as
// rejected, since the owner dropped its commit or took its text out before
// merging. A publisher that can't answer, offline or signed out, leaves its
// reviews pending and joins the error; the rest still refresh. Returns how
// many reviews were decided.
func (l *Ledger) RefreshReviews(ctx context.Context, publisher func(via string) (review.Publisher, error), today time.Time) (int, error) {
	type answer struct {
		v   review.Verdict
		day string
		err error
	}
	asked := map[string]answer{}
	type opened struct {
		p   review.Publisher
		err error
	}
	pubs := map[string]opened{} // one publisher per name
	decided := 0
	var errs []error
	for i := range l.Reviews {
		rv := &l.Reviews[i]
		if rv.Verdict != review.Pending {
			continue
		}
		published := review.Published{Where: rv.Where, Repo: rv.Repo, Branch: rv.Branch, Base: rv.Base, Commit: rv.Commit}
		// A branch name repeats across repos, a pull request URL doesn't,
		// and each proposal's commit is its own question.
		key := rv.Via + "\x00" + rv.Repo + "\x00" + rv.Where + "\x00" + rv.Commit
		pub, ok := pubs[rv.Via]
		if !ok {
			pub.p, pub.err = publisher(rv.Via)
			pubs[rv.Via] = pub
		}
		ans, ok := asked[key]
		if !ok {
			if err := pub.err; err == nil {
				ans.v, ans.day, ans.err = pub.p.Verdict(ctx, published)
			} else {
				ans.err = err
			}
			asked[key] = ans
			if ans.err != nil {
				errs = append(errs, ans.err)
			}
		}
		if ans.err != nil || ans.v == review.Pending {
			continue
		}
		v := ans.v
		// Merged says the change landed, not that this proposal did: the
		// owner can merge a change minus one proposal. The ledger keys each
		// review to the rule text that landed in the staged diff, so a
		// merged add or reword whose text, or a hand rewording of it, is
		// absent from the base was rejected.
		if v == review.Merged && (rv.Kind == "add" || rv.Kind == "reword") && strings.TrimSpace(rv.Rule) != "" {
			var scope review.MergeRange
			if r, isRanger := pub.p.(review.Ranger); isRanger {
				scope, _ = r.Range(ctx, published)
			}
			if landed, ok := review.RuleLanded(ctx, published, rv.Rule, scope); ok && !landed {
				v = review.Closed
			}
		}
		rv.Verdict, rv.Decided = v, ans.day
		if rv.Decided == "" {
			rv.Decided = today.Format("2006-01-02")
		}
		decided++
	}
	return decided, errors.Join(errs...)
}

// LoadLedger reads a ledger file; a missing file is an empty ledger.
func LoadLedger(path string) (*Ledger, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Ledger{}, nil
	}
	if err != nil {
		return nil, err
	}
	var l Ledger
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &l, nil
}

// Save writes the ledger atomically.
func (l *Ledger) Save(path string) error {
	data, err := ledgerBytes(l)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ledgerBytes is the ledger as Save writes it, so comparing two copies'
// bytes says whether they read the same.
func ledgerBytes(l *Ledger) ([]byte, error) {
	sort.Slice(l.Rules, func(i, j int) bool { return l.Rules[i].ID < l.Rules[j].ID })
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func textHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:12]
}

var bulletRE = regexp.MustCompile(`^[-*]\s+(.+)$`)

// Sync is what one SyncTargets pass changed.
type Sync struct {
	Added, Removed, Reworded, Moved int
}

// rewordSimilarity is how alike a vanished rule and a new bullet in the
// same file must be, by their words, to be one rule reworded by hand.
const rewordSimilarity = 0.5

// bullet is one top-level bullet found in a target file.
type bullet struct {
	target, text, hash string
}

// bulletScanner classifies lines the one way every reader of a target file
// reads rules: top-level bullets only, outside code fences, outside a
// leading YAML frontmatter block. A Kiro steering file may open with YAML
// frontmatter between --- lines (its inclusion modes: always, fileMatch
// with fileMatchPattern, manual, auto). A pattern list there is YAML, not a
// rule, so the frontmatter block is skipped whole. line is the 0-based line
// number of the next line scanned; a scanner started mid-file (line > 0)
// never arms frontmatter.
type bulletScanner struct {
	line            int
	inCode, inFront bool
}

// scan reads one line and returns the rule text on it, if the line is a
// top-level bullet.
func (s *bulletScanner) scan(line string) (string, bool) {
	i := s.line
	s.line++
	trimmed := strings.TrimSpace(line)
	if i == 0 && line == "---" {
		s.inFront = true
		return "", false
	}
	if s.inFront {
		if line == "---" || line == "..." {
			s.inFront = false
		}
		return "", false
	}
	if strings.HasPrefix(trimmed, "```") {
		s.inCode = !s.inCode
		return "", false
	}
	if s.inCode {
		return "", false
	}
	m := bulletRE.FindStringSubmatch(trimmed)
	// Top-level bullets only: an indented bullet is detail under a rule.
	if m == nil || line != trimmed {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// scanBullets returns the top-level bullets in one target file's bytes,
// with target left for the caller to fill in.
func scanBullets(data []byte) []bullet {
	var s bulletScanner
	var found []bullet
	for _, line := range strings.Split(string(data), "\n") {
		if text, ok := s.scan(line); ok {
			found = append(found, bullet{text: text, hash: textHash(text)})
		}
	}
	return found
}

// SyncTargets reconciles the ledger with the bullets in the target files.
// A bullet whose text is on the ledger is unchanged. A rule whose text is
// gone and a bullet that is new are matched as one rule, reworded or moved,
// so the rule keeps its id and history: first by the same text in another
// target, then by a reword the loop itself proposed for that rule id, then
// by how alike their words are within one target. What is still unmatched
// is a new rule, or a rule the owner deleted.
func (l *Ledger) SyncTargets(targets []string, today time.Time) (Sync, error) {
	var res Sync
	day := today.Format("2006-01-02")
	var found []bullet
	seen := map[string]bool{}
	absent := map[string]bool{}
	fileDay := map[string]string{}
	for _, target := range targets {
		data, err := os.ReadFile(target)
		if os.IsNotExist(err) {
			// A file that isn't here, such as one another machine holds,
			// says nothing about its rules: they stay as they are.
			absent[target] = true
			continue
		}
		if err != nil {
			return res, err
		}
		if st, err := os.Stat(target); err == nil {
			fileDay[target] = st.ModTime().UTC().Format("2006-01-02")
		}
		for _, b := range scanBullets(data) {
			b.target = target
			if key := b.target + "\x00" + b.hash; !seen[key] {
				seen[key] = true
				found = append(found, b)
			}
		}
	}

	// A target the ledger has never held a rule for is being read for the
	// first time: its bullets were already there, on a day nobody recorded.
	known := map[string]bool{}
	for _, r := range l.Rules {
		known[r.Target] = true
	}

	live := map[string]bool{}
	for _, r := range l.Rules {
		if seen[r.Target+"\x00"+r.TextHash] || absent[r.Target] {
			live[r.ID] = true
		}
	}
	onLedger := map[string]bool{}
	for _, r := range l.Rules {
		onLedger[r.Target+"\x00"+r.TextHash] = true
	}
	var fresh []bullet
	for _, b := range found {
		if !onLedger[b.target+"\x00"+b.hash] {
			fresh = append(fresh, b)
		}
	}
	// dayFor is the day a change to a target took effect: the file's own
	// modification day when it is earlier than the sync day, since a sync
	// may run days after a hand edit, and a session in between must still
	// be judged against what the file said then.
	dayFor := func(target string) string {
		if d := fileDay[target]; d != "" && d < day {
			return d
		}
		return day
	}
	taken := make([]bool, len(fresh))
	claim := func(r *Rule, i int) {
		b := fresh[i]
		taken[i] = true
		live[r.ID] = true
		if r.TextHash != b.hash {
			d := dayFor(b.target)
			r.Supersedes = append(r.Supersedes, Version{TextHash: r.TextHash, Text: r.Text, Until: d})
			r.Text, r.TextHash, r.Since = b.text, b.hash, d
			res.Reworded++
		}
		if r.Target != b.target {
			r.Target = b.target
			res.Moved++
		}
	}

	// The same text in another target: the rule moved.
	for ri := range l.Rules {
		r := &l.Rules[ri]
		for i, b := range fresh {
			if !live[r.ID] && !taken[i] && b.hash == r.TextHash {
				claim(r, i)
			}
		}
	}
	// A reword the loop proposed for this rule id, landed as proposed.
	for ri := range l.Rules {
		r := &l.Rules[ri]
		if live[r.ID] {
			continue
		}
		for _, rv := range l.Reviews {
			if rv.Kind != "reword" || rv.Replaces != r.ID || rv.Verdict == review.Closed {
				continue
			}
			for i, b := range fresh {
				if !live[r.ID] && !taken[i] && ruleHash(b.text) == rv.RuleHash {
					claim(r, i)
					if len(rv.Models) > 0 {
						r.Models = rv.Models // the new wording was earned on these
					}
				}
			}
		}
	}
	// A bullet in the same target that reads like the vanished rule: the
	// most alike pair first, so one edit doesn't steal another's match.
	type pair struct {
		ri, bi int
		sim    float64
	}
	var pairs []pair
	for ri, r := range l.Rules {
		if live[r.ID] {
			continue
		}
		for bi, b := range fresh {
			if taken[bi] || b.target != r.Target {
				continue
			}
			if sim := similarity(r.Text, b.text); sim >= rewordSimilarity {
				pairs = append(pairs, pair{ri, bi, sim})
			}
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].sim > pairs[j].sim })
	for _, p := range pairs {
		if r := &l.Rules[p.ri]; !live[r.ID] && !taken[p.bi] {
			claim(r, p.bi)
		}
	}

	next := max(maxRuleID(l.Rules), l.LastRule)
	kept := l.Rules[:0]
	for _, r := range l.Rules {
		if live[r.ID] {
			kept = append(kept, r)
		} else {
			res.Removed++
		}
	}
	l.Rules = kept
	for i, b := range fresh {
		if taken[i] {
			continue
		}
		next++
		r := Rule{ID: fmt.Sprintf("r-%04d", next), Target: b.target, Text: b.text, TextHash: b.hash, Created: day}
		if known[b.target] {
			r.Since = dayFor(b.target)
		}
		// A bullet the loop proposed carries the sessions that earned it.
		for _, rv := range l.Reviews {
			if rv.Kind == "add" && rv.Verdict != review.Closed && rv.RuleHash == ruleHash(b.text) {
				r.SourceSessions, r.Models = rv.Evidence, rv.Models
				// The loop wrote it, so it is known to be new, even as the
				// first bullet of a file that didn't exist before.
				r.Since = dayFor(b.target)
				break
			}
		}
		l.Rules = append(l.Rules, r)
		res.Added++
	}
	l.LastRule = next
	return res, nil
}

func maxRuleID(rules []Rule) int {
	max := 0
	for _, r := range rules {
		var n int
		if _, err := fmt.Sscanf(r.ID, "r-%d", &n); err == nil && n > max {
			max = n
		}
	}
	return max
}

// ApplyHits folds the map step's rule hits into the ledger, each dated by
// the day its session ran, not the day it was mapped. A hit counts only for
// a rule that has read the way the model was shown it: another ledger's
// rule under the same id is dropped. Hits inside an observation record are
// from before judgments had their own records, and match by id alone.
//
// A rule with any hit here has its dates recomputed from these hits alone,
// never folded forward from the ledger's old values: the store keeps every
// judgment, so a session re-read with its own time (`recall learn map
// --since all`) moves a date stamped at map time back to the day the
// session ran. Call it once with everything, not once per batch.
func (l *Ledger) ApplyHits(observed []SessionObservations, judged []SessionHits) {
	byID := map[string]*Rule{}
	for i := range l.Rules {
		byID[l.Rules[i].ID] = &l.Rules[i]
	}
	type dates struct{ relevant, violated string }
	latest := map[*Rule]*dates{}
	touch := func(r *Rule) *dates {
		if latest[r] == nil {
			latest[r] = &dates{}
		}
		return latest[r]
	}
	apply := func(hit RuleHit, day string) {
		r, ok := byID[hit.ID]
		if !ok || (hit.TextHash != "" && !r.Wrote(hit.TextHash)) {
			return
		}
		switch hit.Status {
		case "relevant", "followed":
			if d := touch(r); day > d.relevant {
				d.relevant = day
			}
		case "violated":
			d := touch(r)
			if day > d.violated {
				d.violated = day
			}
			if day > d.relevant {
				d.relevant = day
			}
		}
	}
	for _, so := range observed {
		for _, hit := range so.RuleHits {
			apply(hit, so.Day())
		}
	}
	for _, sh := range judged {
		for _, hit := range sh.Hits {
			apply(hit, sh.Day())
		}
	}
	for r, d := range latest {
		r.LastRelevant, r.LastViolated = d.relevant, d.violated
	}
}

// Outgrown returns the rules earned only on models that no recent session
// runs, given the models recent sessions do run. Each is a rule the current
// model may follow unprompted: a candidate for a trial without it, never a
// retirement on this alone. A rule that records no model isn't listed.
func (l *Ledger) Outgrown(current []string) []Rule {
	if len(current) == 0 {
		return nil
	}
	now := map[string]bool{}
	for _, m := range current {
		now[m] = true
	}
	var out []Rule
	for _, r := range l.Rules {
		if len(r.Models) == 0 {
			continue
		}
		still := false
		for _, m := range r.Models {
			still = still || now[m]
		}
		if !still {
			out = append(out, r)
		}
	}
	return out
}

// Aging is the deterministic policy from docs/learning.md.
type Aging struct {
	Reword []Rule // violated: not working as written
	Retire []Rule // no relevance for the window
}

// Age reports which rules want rewording or retirement. A rule wants
// rewording when it was violated inside the window as it reads now: a
// violation of the wording it replaced doesn't count against the new one.
// A rule is stale when neither its creation nor its last relevance is
// within retireDays.
func (l *Ledger) Age(today time.Time, retireDays int) Aging {
	cutoff := today.AddDate(0, 0, -retireDays).Format("2006-01-02")
	var a Aging
	for _, r := range l.Rules {
		switch {
		case r.LastViolated != "" && r.LastViolated >= cutoff && r.LastViolated > r.Since:
			a.Reword = append(a.Reword, r)
		case r.LastRelevant >= cutoff || r.Created >= cutoff:
			// earned its place
		default:
			a.Retire = append(a.Retire, r)
		}
	}
	return a
}
