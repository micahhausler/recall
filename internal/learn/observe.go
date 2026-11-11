package learn

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/store"
)

//go:embed prompts/map.md
var mapPrompt string

//go:embed prompts/reduce.md
var reducePrompt string

// Observation is one structured item the map step extracted.
type Observation struct {
	Category string `json:"category"`
	Text     string `json:"text"`
	Quote    string `json:"quote,omitempty"`
}

// RuleHit is the map step reporting an existing rule mattered in a session.
type RuleHit struct {
	ID     string `json:"id"`
	Status string `json:"status"` // relevant, followed, or violated
	Note   string `json:"note,omitempty"`
}

// SessionObservations is the cached map output for one session.
type SessionObservations struct {
	Fingerprint  string        `json:"fingerprint"`
	Harness      string        `json:"harness"`
	Host         string        `json:"host,omitempty"`
	Session      string        `json:"session"`
	Model        string        `json:"model"`
	ObservedAt   string        `json:"observed_at"`
	CostUSD      float64       `json:"cost_usd,omitempty"`
	Observations []Observation `json:"observations"`
	RuleHits     []RuleHit     `json:"rule_hits,omitempty"`
}

// Fingerprint keys the cache: the rendered session, the prompt, the model,
// and the rule ids shown. Any change re-observes; a match skips.
func Fingerprint(rendered, mapModel string, ruleIDs []string) string {
	h := sha256.New()
	for _, part := range []string{rendered, mapPrompt, mapModel, strings.Join(ruleIDs, ",")} {
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Key is where one session's observations live in the store.
func Key(harness, session, fingerprint string) string {
	return fmt.Sprintf("analysis/%s/%s-%s.json", harness, session, fingerprint)
}

// Mapper runs the map step: one cheap model call per new or changed session,
// cached in the store.
type Mapper struct {
	Store  store.Store
	Runner model.Runner
	Model  string
	Host   string
	// Rules are the current rules shown to the model for rule hits.
	Rules []Rule
	Clock clock.Clock
	Out   io.Writer
	// Usage accumulates across MapSession calls that ran the model.
	Usage model.Usage
}

// MapSession observes one session's events, reusing the cache when the
// fingerprint matches. The bool reports whether the model ran.
func (m *Mapper) MapSession(ctx context.Context, harnessName, session string, evs []events.Event) (*SessionObservations, bool, error) {
	rendered := Render(evs)
	ids := make([]string, len(m.Rules))
	for i, r := range m.Rules {
		ids[i] = r.ID
	}
	fp := Fingerprint(rendered, m.Model, ids)
	key := Key(harnessName, session, fp)
	if r, err := m.Store.Get(ctx, key); err == nil {
		defer r.Close()
		var cached SessionObservations
		if json.NewDecoder(r).Decode(&cached) == nil {
			return &cached, false, nil
		}
	} else if !errors.Is(err, store.ErrNotExist) {
		return nil, false, err
	}
	if !strings.Contains(rendered, "[owner] ") {
		// No owner prompts: a scripted run or subagent noise. Nothing to
		// learn from a person here; cache the empty result without a call.
		so := &SessionObservations{Fingerprint: fp, Harness: harnessName, Host: m.Host,
			Session: session, Model: m.Model, ObservedAt: m.Clock.Now().UTC().Format(time.RFC3339)}
		return so, false, m.put(ctx, key, so)
	}

	input := mapPrompt + "\n\n# SESSION\n\n" + rendered
	if len(m.Rules) > 0 {
		var b strings.Builder
		b.WriteString("\n\n# CURRENT RULES\n\n")
		for _, r := range m.Rules {
			fmt.Fprintf(&b, "Rule[%s]: %s\n", r.ID, r.Text)
		}
		input += b.String()
	}
	out, usage, err := m.Runner.Run(ctx, m.Model, input)
	if err != nil {
		return nil, false, fmt.Errorf("map %s/%s: %w", harnessName, session, err)
	}
	m.Usage = m.Usage.Add(usage)
	obs, hits := ParseObservations(out)
	so := &SessionObservations{
		Fingerprint: fp, Harness: harnessName, Host: m.Host, Session: session,
		Model: m.Model, ObservedAt: m.Clock.Now().UTC().Format(time.RFC3339),
		CostUSD:      usage.CostUSD,
		Observations: obs, RuleHits: hits,
	}
	return so, true, m.put(ctx, key, so)
}

func (m *Mapper) put(ctx context.Context, key string, so *SessionObservations) error {
	data, err := json.MarshalIndent(so, "", "  ")
	if err != nil {
		return err
	}
	err = m.Store.PutIfAbsent(ctx, key, strings.NewReader(string(data)))
	if errors.Is(err, store.ErrExists) {
		return nil // a concurrent run won; same fingerprint, same content
	}
	return err
}

// Categories the map prompt allows; anything else is discarded at parse time.
var categories = map[string]bool{
	"correction": true, "context": true, "procedure": true,
	"tool-failure": true, "skill-miss": true,
}

var (
	obsRE   = regexp.MustCompile(`^Observation\[([a-z-]+)\]:\s*(.+)$`)
	quoteRE = regexp.MustCompile(`^Quote:\s*"?(.*?)"?\s*$`)
	ruleRE  = regexp.MustCompile(`^Rule\[([^\]]+)\]:\s*(relevant|followed|violated)\s*(?:[-—–]\s*(.*))?$`)
)

// ParseObservations reads the map model's output. The structured prefixes
// are the contract: lines in any other shape (including prose around NONE)
// are discarded, which converts "did the model find nothing?" from a
// semantic judgment into a parse rule.
func ParseObservations(out string) ([]Observation, []RuleHit) {
	var obs []Observation
	var hits []RuleHit
	quote := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if m := quoteRE.FindStringSubmatch(line); m != nil && strings.HasPrefix(line, "Quote:") {
			quote = m[1]
			continue
		}
		if m := obsRE.FindStringSubmatch(line); m != nil {
			if categories[m[1]] {
				obs = append(obs, Observation{Category: m[1], Text: strings.TrimSpace(m[2]), Quote: quote})
			}
			quote = ""
			continue
		}
		if m := ruleRE.FindStringSubmatch(line); m != nil {
			hits = append(hits, RuleHit{ID: m[1], Status: m[2], Note: strings.TrimSpace(m[3])})
		}
		quote = ""
	}
	return obs, hits
}

// ListObservations loads every cached observation under a harness prefix.
func ListObservations(ctx context.Context, st store.Store, harnessName string) ([]SessionObservations, error) {
	keys, err := st.List(ctx, "analysis/"+harnessName+"/")
	if err != nil {
		return nil, err
	}
	// A changed session leaves its old fingerprint behind (the store never
	// deletes), so keep only the newest observation per session.
	newest := map[string]SessionObservations{}
	var order []string
	for _, key := range keys {
		r, err := st.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		var so SessionObservations
		err = json.NewDecoder(r).Decode(&so)
		r.Close()
		if err != nil {
			continue // a torn or foreign object; skip, don't fail the window
		}
		prev, seen := newest[so.Session]
		if !seen {
			order = append(order, so.Session)
		}
		if !seen || so.ObservedAt > prev.ObservedAt {
			newest[so.Session] = so
		}
	}
	out := make([]SessionObservations, 0, len(order))
	for _, s := range order {
		out = append(out, newest[s])
	}
	return out, nil
}
