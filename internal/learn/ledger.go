package learn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/review"
)

// Rule is one standing rule on the ledger. The ledger is a sidecar file next
// to the context files, never comments inside them, because comments cost
// context in every session.
type Rule struct {
	ID             string   `json:"id"`
	Target         string   `json:"target"`
	Text           string   `json:"text"`
	TextHash       string   `json:"text_hash"`
	SourceSessions []string `json:"source_sessions,omitempty"`
	Created        string   `json:"created"`
	LastRelevant   string   `json:"last_relevant,omitempty"`
	LastViolated   string   `json:"last_violated,omitempty"`
}

// Ledger is every rule recall tracks, and every proposal an apply sent for
// review with the owner's verdict on it.
type Ledger struct {
	Rules   []Rule   `json:"rules"`
	Reviews []Review `json:"reviews,omitempty"`
}

// Review is one proposal an apply sent for review. A closed one is the
// owner saying no, which the reduce reads so it doesn't ask again.
type Review struct {
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Rule     string `json:"rule"`
	RuleHash string `json:"rule_hash"`
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
	Verdict string `json:"verdict"`
	Decided string `json:"decided,omitempty"`
}

// Place says where the review is for a person: a pull request URL, or a
// branch and the repo it's in.
func (rv Review) Place() string {
	if strings.Contains(rv.Where, "://") || rv.Repo == "" {
		return rv.Where
	}
	return rv.Where + " in " + rv.Repo
}

// RecordReviews adds a pending review for each proposal a finished apply
// published. Recording the same run twice adds nothing.
func (l *Ledger) RecordReviews(plan *Plan, res *ApplyResult, via string) int {
	key := func(repo, where, title string) string { return repo + "\x00" + where + "\x00" + title }
	seen := map[string]bool{}
	for _, rv := range l.Reviews {
		seen[key(rv.Repo, rv.Where, rv.Title)] = true
	}
	added := 0
	for _, c := range res.Changes {
		if c.Error != "" || c.Where == "" {
			continue // not in front of the owner
		}
		for _, title := range c.Proposals {
			if seen[key(c.Repo, c.Where, title)] {
				continue
			}
			rv := Review{Title: title, Proposed: plan.Date, Via: via, Where: c.Where, Repo: c.Repo,
				Branch: c.Branch, Base: c.Base, Commit: c.Commit, Verdict: string(review.Pending)}
			for _, r := range plan.Routes {
				if r.Skip == "" && r.Title == title {
					rv.Kind, rv.Rule, rv.RuleHash = r.Kind, r.Rule, ruleHash(r.Rule)
					break
				}
			}
			l.Reviews = append(l.Reviews, rv)
			seen[key(c.Repo, c.Where, title)] = true
			added++
		}
	}
	return added
}

// RefreshReviews asks each pending review's publisher what became of it,
// once per place a change landed. A publisher that can't answer, offline
// or signed out, leaves its reviews pending and joins the error; the rest
// still refresh. Returns how many reviews were decided.
func (l *Ledger) RefreshReviews(ctx context.Context, publisher func(via string) (review.Publisher, error), today time.Time) (int, error) {
	type answer struct {
		v   review.Verdict
		day string
		err error
	}
	asked := map[string]answer{}
	decided := 0
	var errs []error
	for i := range l.Reviews {
		rv := &l.Reviews[i]
		if rv.Verdict != string(review.Pending) {
			continue
		}
		// A branch name repeats across repos; a pull request URL doesn't.
		key := rv.Via + "\x00" + rv.Repo + "\x00" + rv.Where
		ans, ok := asked[key]
		if !ok {
			p, err := publisher(rv.Via)
			if err == nil {
				ans.v, ans.day, ans.err = p.Verdict(ctx, review.Published{Where: rv.Where, Repo: rv.Repo,
					Branch: rv.Branch, Base: rv.Base, Commit: rv.Commit})
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
		rv.Verdict, rv.Decided = string(ans.v), ans.day
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
	sort.Slice(l.Rules, func(i, j int) bool { return l.Rules[i].ID < l.Rules[j].ID })
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func textHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:12]
}

var bulletRE = regexp.MustCompile(`^[-*]\s+(.+)$`)

// SyncTargets reconciles the ledger with the bullets in the target files:
// a new bullet gets a rule id, a bullet whose text changed gets a fresh
// entry (history stays by id), and a rule whose bullet is gone is removed
// from the ledger (the owner already deleted the rule itself). Returns
// added, removed.
func (l *Ledger) SyncTargets(targets []string, today time.Time) (added, removed int, err error) {
	live := map[string]bool{} // target + text hash still present
	next := maxRuleID(l.Rules)
	byHash := map[string]bool{}
	for _, r := range l.Rules {
		byHash[r.Target+"\x00"+r.TextHash] = true
	}
	for _, target := range targets {
		data, err := os.ReadFile(target)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, 0, err
		}
		inCode := false
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "```") {
				inCode = !inCode
				continue
			}
			if inCode {
				continue
			}
			m := bulletRE.FindStringSubmatch(trimmed)
			// Top-level bullets only: an indented bullet is detail under a rule.
			if m == nil || line != trimmed {
				continue
			}
			text := strings.TrimSpace(m[1])
			h := textHash(text)
			live[target+"\x00"+h] = true
			if byHash[target+"\x00"+h] {
				continue
			}
			next++
			l.Rules = append(l.Rules, Rule{
				ID: fmt.Sprintf("r-%04d", next), Target: target, Text: text, TextHash: h,
				Created: today.Format("2006-01-02"),
			})
			byHash[target+"\x00"+h] = true
			added++
		}
	}
	kept := l.Rules[:0]
	for _, r := range l.Rules {
		if live[r.Target+"\x00"+r.TextHash] {
			kept = append(kept, r)
		} else {
			removed++
		}
	}
	l.Rules = kept
	return added, removed, nil
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

// ApplyHits folds the map step's rule hits into the ledger.
func (l *Ledger) ApplyHits(all []SessionObservations) {
	byID := map[string]*Rule{}
	for i := range l.Rules {
		byID[l.Rules[i].ID] = &l.Rules[i]
	}
	for _, so := range all {
		day := so.ObservedAt
		if len(day) >= 10 {
			day = day[:10]
		}
		for _, hit := range so.RuleHits {
			r, ok := byID[hit.ID]
			if !ok {
				continue // the model invented an id; drop it
			}
			switch hit.Status {
			case "relevant", "followed":
				if day > r.LastRelevant {
					r.LastRelevant = day
				}
			case "violated":
				if day > r.LastViolated {
					r.LastViolated = day
				}
				if day > r.LastRelevant {
					r.LastRelevant = day
				}
			}
		}
	}
}

// Aging is the deterministic policy from docs/learning.md.
type Aging struct {
	Reword []Rule // violated: not working as written
	Retire []Rule // no relevance for the window
}

// Age reports which rules want rewording or retirement. A rule is stale when
// neither its creation nor its last relevance is within retireDays.
func (l *Ledger) Age(today time.Time, retireDays int) Aging {
	cutoff := today.AddDate(0, 0, -retireDays).Format("2006-01-02")
	var a Aging
	for _, r := range l.Rules {
		switch {
		case r.LastViolated != "" && r.LastViolated >= cutoff:
			a.Reword = append(a.Reword, r)
		case r.LastRelevant >= cutoff || r.Created >= cutoff:
			// earned its place
		default:
			a.Retire = append(a.Retire, r)
		}
	}
	return a
}
