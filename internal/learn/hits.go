package learn

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/micahhausler/recall/internal/store"
)

//go:embed prompts/hits.md
var hitsPrompt string

//go:embed prompts/hits-only.md
var hitsOnlyPrompt string

// RuleRef names one wording of one rule.
type RuleRef struct {
	ID       string `json:"id"`
	TextHash string `json:"text_hash"`
}

// SessionHits is one judgment of a session against a set of rules: which
// wordings the model was shown, and which of them the session touched. It
// is stored apart from the session's observations, so a rule change never
// re-observes a session.
type SessionHits struct {
	Fingerprint string `json:"fingerprint"`
	Harness     string `json:"harness"`
	Host        string `json:"host,omitempty"`
	Session     string `json:"session"`
	// Content is the hash of the rendered session that was judged, Prompt
	// the hash of the rule-hit prompt; a judgment counts only for both.
	Content    string  `json:"content"`
	Prompt     string  `json:"prompt"`
	Model      string  `json:"model"`
	JudgedAt   string  `json:"judged_at"`
	FirstEvent string  `json:"first_event,omitempty"`
	LastEvent  string  `json:"last_event,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	// Rules are the wordings shown; a rule listed here with no hit was
	// judged and not touched.
	Rules []RuleRef `json:"rules"`
	Hits  []RuleHit `json:"hits,omitempty"`
	// Partial is set when only a prefix of a refused session was judged;
	// Refused when the model declined to judge it at all.
	Partial *Partial `json:"partial,omitempty"`
	Refused bool     `json:"refused,omitempty"`
}

// Day is the day (UTC) the judged session last ran.
func (sh SessionHits) Day() string {
	if len(sh.LastEvent) >= 10 {
		return sh.LastEvent[:10]
	}
	if len(sh.JudgedAt) >= 10 {
		return sh.JudgedAt[:10]
	}
	return ""
}

func shortHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// contentHash keys what the model read of a session.
func contentHash(rendered string) string { return shortHash(rendered) }

// hitsPromptHash keys the rule-hit instructions a judgment followed.
func hitsPromptHash() string { return shortHash(hitsOnlyPrompt, hitsPrompt) }

func refs(rules []Rule) []RuleRef {
	out := make([]RuleRef, len(rules))
	for i, r := range rules {
		out[i] = RuleRef{ID: r.ID, TextHash: r.TextHash}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// hitsPrefix is where one session's judgments live.
func hitsPrefix(harness, session string) string {
	return fmt.Sprintf("analysis/hits/%s/%s-", harness, session)
}

// HitsKey is where one judgment lives in the store.
func HitsKey(harness, session, content, mapModel string, shown []RuleRef) string {
	parts := []string{content, hitsPromptHash(), mapModel}
	for _, r := range shown {
		parts = append(parts, r.ID+":"+r.TextHash)
	}
	return hitsPrefix(harness, session) + shortHash(parts...) + ".json"
}

// priorHits loads the judgments already made of this session content with
// this model and prompt.
func priorHits(ctx context.Context, st store.Store, harness, session, content, mapModel string) ([]SessionHits, error) {
	keys, err := st.List(ctx, hitsPrefix(harness, session))
	if err != nil {
		return nil, err
	}
	var out []SessionHits
	for _, key := range keys {
		sh, ok, err := getJSON[SessionHits](ctx, st, key)
		if err != nil {
			return nil, err
		}
		if ok && sh.Session == session && sh.Content == content && sh.Model == mapModel && sh.Prompt == hitsPromptHash() {
			out = append(out, sh)
		}
	}
	return out, nil
}

// inForce reports whether the rule's current wording existed when a session
// that last ran on day was live. A session can't follow or break a rule
// that wasn't written yet, and a session with no readable time of its own
// (an empty day) is in force for no rule: nothing says which rules existed
// when it ran, so it is skipped for hits rather than judged against all.
func inForce(r Rule, day string) bool {
	return day != "" && (r.Since == "" || r.Since <= day)
}

// unjudged returns the rules a session still has to be judged against: the
// ones in force when it ran that no earlier judgment of this content was
// shown.
func unjudged(rules []Rule, prior []SessionHits, day string) []Rule {
	shown := map[RuleRef]bool{}
	for _, sh := range prior {
		for _, r := range sh.Rules {
			shown[r] = true
		}
	}
	var out []Rule
	for _, r := range rules {
		if inForce(r, day) && !shown[RuleRef{ID: r.ID, TextHash: r.TextHash}] {
			out = append(out, r)
		}
	}
	return out
}

// known keeps the hits that name a rule the model was shown, stamped with
// the wording it was shown; an id the model invented is dropped.
func known(hits []RuleHit, shown []Rule) []RuleHit {
	hash := map[string]string{}
	for _, r := range shown {
		hash[r.ID] = r.TextHash
	}
	var out []RuleHit
	for _, h := range hits {
		if th, ok := hash[h.ID]; ok {
			h.TextHash = th
			out = append(out, h)
		}
	}
	return out
}

// ListHits loads every judgment under a harness prefix.
func ListHits(ctx context.Context, st store.Store, harnessName string) ([]SessionHits, error) {
	keys, err := st.List(ctx, "analysis/hits/"+harnessName+"/")
	if err != nil {
		return nil, err
	}
	var out []SessionHits
	for _, key := range keys {
		sh, ok, err := getJSON[SessionHits](ctx, st, key)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, sh)
		}
	}
	return out, nil
}

// getJSON reads one stored JSON object. ok is false for a missing key and
// for a torn or foreign object, which is skipped rather than failing a run.
// A record written by the removed client-side encryption feature is an error
// naming the object, never a silent skip.
func getJSON[T any](ctx context.Context, st store.Store, key string) (v T, ok bool, err error) {
	if strings.HasSuffix(key, ".age") {
		return v, false, fmt.Errorf("%s is encrypted by a feature recall removed; decrypt it with the age CLI and store the plain record beside it", key)
	}
	r, err := st.Get(ctx, key)
	if err != nil {
		if errors.Is(err, store.ErrNotExist) {
			return v, false, nil
		}
		return v, false, err
	}
	defer r.Close()
	if json.NewDecoder(r).Decode(&v) != nil {
		return v, false, nil
	}
	return v, true, nil
}

func putJSON(ctx context.Context, st store.Store, key string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	err = st.PutIfAbsent(ctx, key, strings.NewReader(string(data)))
	if errors.Is(err, store.ErrExists) {
		return nil // a concurrent run won; same key, same content
	}
	return err
}
