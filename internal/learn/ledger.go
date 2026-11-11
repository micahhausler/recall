package learn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
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

// Ledger is every rule recall tracks.
type Ledger struct {
	Rules []Rule `json:"rules"`
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
