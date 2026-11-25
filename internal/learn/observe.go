package learn

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/redact"
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
	// Quote is the owner's words behind the observation, kept only when
	// the binary found them in a prompt the owner typed; Source is then
	// SourceOwner.
	Quote  string `json:"quote,omitempty"`
	Source string `json:"source,omitempty"`
	// Origin is the session a native memory note came from, when the note
	// says: evidence from the note and from that session counts once.
	Origin string `json:"origin,omitempty"`
}

// RuleHit is the map step reporting an existing rule mattered in a session.
type RuleHit struct {
	ID string `json:"id"`
	// TextHash is the wording the model was shown, set by the binary, so a
	// hit never lands on a different rule that happens to share the id.
	TextHash string `json:"text_hash,omitempty"`
	Status   string `json:"status"` // relevant, followed, or violated
	Note     string `json:"note,omitempty"`
}

// SessionObservations is the cached map output for one session.
type SessionObservations struct {
	Fingerprint string `json:"fingerprint"`
	Harness     string `json:"harness"`
	Host        string `json:"host,omitempty"`
	Session     string `json:"session"`
	// Model is the map model as configured, Effort the level it ran at
	// (empty for the harness default), and ModelID the model the harness
	// resolved Model to, when it reports one.
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
	ModelID string `json:"model_id,omitempty"`
	// ObservedAt is when the map step read the session; FirstEvent and
	// LastEvent are when the session itself ran. Evidence and rule hits are
	// dated by the session, so mapping an old session today is not new
	// evidence.
	ObservedAt string `json:"observed_at"`
	FirstEvent string `json:"first_event,omitempty"`
	LastEvent  string `json:"last_event,omitempty"`
	// Scope is where the session ran.
	Scope Scope `json:"scope,omitzero"`
	// Prompts is how many prompts the owner typed, and Signals the
	// session's first look-around actions, which the binary counts across
	// sessions to find an agent rediscovering the same layout.
	Prompts int      `json:"prompts,omitempty"`
	Signals []Signal `json:"signals,omitempty"`
	// Failures are the session's distinct tool failures, by tool and
	// normalized error text, skipping expected nonzero exits; the binary
	// counts them across sessions (learn.repeat_evidence_sessions).
	Failures []Signal `json:"failures,omitempty"`
	// Models are the models that answered in the session. A rule is earned
	// against the model that needed it, and a later model may not.
	Models       []string      `json:"models,omitempty"`
	CostUSD      float64       `json:"cost_usd,omitempty"`
	Observations []Observation `json:"observations"`
	// RuleHits is set only on records written before rule hits moved to
	// their own records (SessionHits).
	RuleHits []RuleHit `json:"rule_hits,omitempty"`
	// Partial is set when the model refused the whole session and only a
	// prefix of it was observed.
	Partial *Partial `json:"partial,omitempty"`
}

// Dated reports whether the record carries the session's own time. One
// written before session dating doesn't, and falls back to its map time.
func (so SessionObservations) Dated() bool { return len(so.LastEvent) >= 10 }

// Day is the day (UTC) the session last ran, which dates its evidence and
// rule hits. A record without session times answers with the day it was
// mapped, the only date it has.
func (so SessionObservations) Day() string {
	if so.Dated() {
		return so.LastEvent[:10]
	}
	if len(so.ObservedAt) >= 10 {
		return so.ObservedAt[:10]
	}
	return ""
}

// span returns the first and last event times of a session, in UTC. Events
// without a readable timestamp don't count.
func span(evs []events.Event) (first, last string) {
	var lo, hi time.Time
	for _, e := range evs {
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			continue
		}
		if lo.IsZero() || t.Before(lo) {
			lo = t
		}
		if t.After(hi) {
			hi = t
		}
	}
	if lo.IsZero() {
		return "", ""
	}
	return lo.UTC().Format(time.RFC3339), hi.UTC().Format(time.RFC3339)
}

// Partial records how much of a refused session the map step could read.
type Partial struct {
	// Prompts of Of owner prompts were observed; 0 means none were.
	Prompts int    `json:"prompts"`
	Of      int    `json:"of"`
	Reason  string `json:"reason"`
}

// Fingerprint keys the observation cache: the rendered session, the prompt,
// and the map model and effort. Any change re-observes; a match skips. The
// rules aren't part of it: a rule change re-judges rule hits and never
// re-observes. An empty effort adds nothing, so a store mapped without one
// keeps its keys.
func Fingerprint(rendered string, mapCall model.Call) string {
	return shortHash(withEffort([]string{rendered, mapPrompt, mapCall.Model}, mapCall.Effort)...)
}

// withEffort appends effort to a cache key's parts when it is set.
func withEffort(parts []string, effort string) []string {
	if effort == "" {
		return parts
	}
	return append(parts, "effort:"+effort)
}

// Key is where one session's observations live in the store.
func Key(harness, session, fingerprint string) string {
	return fmt.Sprintf("analysis/%s/%s-%s.json", harness, session, fingerprint)
}

// Mapper runs the map step: one cheap model call per new or changed session,
// cached in the store. A session read for the first time is observed and
// judged against the rules in one call; the two results are stored apart.
type Mapper struct {
	Store  store.Store
	Runner model.Runner
	Model  string
	// Effort is the level the map model runs at; empty for the harness
	// default.
	Effort string
	Host   string
	// Rules are the current rules a session is judged against.
	Rules []Rule
	// Home is this machine's home directory and Locate finds the checkout
	// that holds a directory here. Both are set only when the sessions
	// being mapped ran on this machine; another host's paths mean nothing
	// here.
	Home   string
	Locate Locator
	// Scoping limits a repo's own rules to the sessions that ran in that
	// repo; nil judges every session against every rule.
	Scoping *Scoping
	// Redact, when set (archive-chats.redact), runs observation text and
	// quotes through the redactor before they are written to the store.
	Redact bool
	// mu guards Usage; one Mapper may map several sessions at once.
	mu    sync.Mutex
	Clock clock.Clock
	Out   io.Writer
	// Usage accumulates across MapSession calls that ran the model.
	Usage model.Usage
}

// Mapped is what MapSession found or made for one session.
type Mapped struct {
	Observations *SessionObservations
	// Hits is the judgment this call made; nil when no rule needed judging.
	Hits *SessionHits
	// Observed and Judged report which model work this call did; both are
	// false for a session answered wholly from the cache.
	Observed, Judged bool
	CostUSD          float64
}

// Ran reports whether the model ran.
func (m *Mapped) Ran() bool { return m.Observed || m.Judged }

// mapCall is the model and effort the map runs.
func (m *Mapper) mapCall() model.Call { return model.Call{Model: m.Model, Effort: m.Effort} }

// plan is what one session needs from the model.
type plan struct {
	rendered, content, obsKey string
	first, last               string
	scope                     Scope
	prompts                   int
	signals                   []Signal
	failures                  []Signal
	models                    []string
	cached                    *SessionObservations
	// judge are the rules still to judge this content against.
	judge []Rule
	// owner is false for a session no person typed into.
	owner bool
}

// selfPrompts open every input the loop sends through a runner; a session
// whose first prompt starts with one is recall's own model run read back as
// a transcript, not a session the owner typed into.
var selfPrompts = []string{mapPrompt, hitsOnlyPrompt, reducePrompt, applyPrompt, replayPrompt}

// selfAuthored reports whether the session is one of recall's own model
// runs: its first owner prompt starts with one of recall's embedded
// prompts, verbatim. Claude runs are unsaved and hook-guarded, but a
// harness that saves every run, like Kiro, can leak one when the run
// crashes before its runner learns the session's id; counting a leaked run
// as the owner would feed the loop its own prompts as owner evidence.
func selfAuthored(evs []events.Event) bool {
	for _, e := range evs {
		if e.Kind != events.UserPrompt || e.Sidechain {
			continue
		}
		text := strings.TrimSpace(e.Text)
		for _, p := range selfPrompts {
			if strings.HasPrefix(text, strings.TrimSpace(p)) {
				return true
			}
		}
		// Only the first prompt decides: a leaked run's input is its
		// first and only prompt, and an owner quoting a prompt later in
		// a real session is still the owner.
		return false
	}
	return false
}

func (m *Mapper) plan(ctx context.Context, harnessName, session string, evs []events.Event) (*plan, error) {
	p := &plan{rendered: Render(evs)}
	p.content = contentHash(p.rendered)
	p.obsKey = Key(harnessName, session, Fingerprint(p.rendered, m.mapCall()))
	p.first, p.last = span(evs)
	p.scope = scopeOf(evs, m.Home, m.Locate)
	p.signals = signalsOf(evs, m.Home)
	p.failures = failuresOf(evs, m.Home)
	seenModel := map[string]bool{}
	for _, e := range evs {
		if e.Kind == events.UserPrompt && !e.Sidechain {
			p.prompts++
		}
		// A harness writes a placeholder like <synthetic> for a turn no
		// model produced.
		if !e.Sidechain && e.Model != "" && !strings.HasPrefix(e.Model, "<") && !seenModel[e.Model] {
			seenModel[e.Model] = true
			p.models = append(p.models, e.Model)
		}
	}
	p.owner = strings.Contains(p.rendered, "[owner] ") && !selfAuthored(evs)
	cached, ok, err := getJSON[SessionObservations](ctx, m.Store, p.obsKey)
	if err != nil {
		return nil, err
	}
	if ok {
		p.cached = &cached
	}
	if !p.owner || len(m.Rules) == 0 {
		return p, nil
	}
	day := ""
	if len(p.last) >= 10 {
		day = p.last[:10]
	}
	var rules []Rule
	for _, r := range m.Rules {
		if m.Scoping.Applies(r.Target, p.scope) {
			rules = append(rules, r)
		}
	}
	// candidate is what this session would be judged against had nothing
	// been judged yet. When it is empty, no listing can add to it; when a
	// judgment of this content against exactly these rules exists, its key
	// is deterministic, so one Head answers the common fully-cached case
	// without a List and a Get per prior judgment. Only a miss (rules
	// judged piecemeal across earlier records) pays to read them all.
	candidate := unjudged(rules, nil, day)
	if len(candidate) == 0 {
		return p, nil
	}
	if _, err := m.Store.Head(ctx, HitsKey(harnessName, session, p.content, m.mapCall(), refs(candidate))); err == nil {
		return p, nil
	}
	prior, err := priorHits(ctx, m.Store, harnessName, session, p.content, m.mapCall())
	if err != nil {
		return nil, err
	}
	p.judge = unjudged(rules, prior, day)
	return p, nil
}

// MapSession observes one session's events and judges it against the rules,
// reusing what the store already holds: observations by the session's
// content, and rule hits by the content and the rule wordings shown.
func (m *Mapper) MapSession(ctx context.Context, harnessName, session string, evs []events.Event) (*Mapped, error) {
	p, err := m.plan(ctx, harnessName, session, evs)
	if err != nil {
		return nil, err
	}
	res := &Mapped{Observations: p.cached}
	now := m.Clock.Now().UTC().Format(time.RFC3339)
	if !p.owner {
		// No owner prompts: a scripted run or subagent noise. Nothing to
		// learn from a person here; cache the empty result without a call.
		if p.cached == nil {
			res.Observations = &SessionObservations{Fingerprint: Fingerprint(p.rendered, m.mapCall()), Harness: harnessName,
				Host: m.Host, Session: session, Model: m.Model, Effort: m.Effort, ObservedAt: now, FirstEvent: p.first, LastEvent: p.last,
				Scope: p.scope, Prompts: p.prompts, Signals: p.signals, Failures: p.failures, Models: p.models}
			return res, putJSON(ctx, m.Store, p.obsKey, res.Observations)
		}
		return res, nil
	}
	if p.cached != nil && len(p.judge) == 0 {
		return res, nil
	}

	observe := p.cached == nil
	var partial *Partial
	refused := false
	var modelID string
	out, err := m.call(ctx, m.input(p.rendered, observe, p.judge), &res.CostUSD, &modelID)
	if errors.Is(err, model.ErrRefusal) {
		if observe {
			out, partial, err = m.salvage(ctx, evs, p.judge, &res.CostUSD, &modelID)
		} else {
			// The observations are cached; record the refusal so later
			// runs don't pay to be refused again.
			out, refused, err = "", true, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("map %s/%s: %w", harnessName, session, err)
	}
	obs, hits := ParseObservations(out)
	obs = verifyQuotes(obs, evs)
	if m.Redact {
		// After the quote check, which needs the words as typed; the quote
		// stays owner-typed with the token replaced.
		for i := range obs {
			obs[i].Text, _ = redact.String(obs[i].Text)
			obs[i].Quote, _ = redact.String(obs[i].Quote)
		}
	}
	if observe {
		res.Observed = true
		res.Observations = &SessionObservations{
			Fingerprint: Fingerprint(p.rendered, m.mapCall()), Harness: harnessName, Host: m.Host, Session: session,
			Model: m.Model, Effort: m.Effort, ModelID: modelID, ObservedAt: now, FirstEvent: p.first, LastEvent: p.last,
			Scope: p.scope, Prompts: p.prompts, Signals: p.signals, Failures: p.failures, Models: p.models,
			CostUSD: res.CostUSD, Observations: obs, Partial: partial,
		}
		if err := putJSON(ctx, m.Store, p.obsKey, res.Observations); err != nil {
			return nil, err
		}
	}
	if len(p.judge) > 0 {
		shown := refs(p.judge)
		res.Judged = true
		res.Hits = &SessionHits{
			Harness: harnessName, Host: m.Host, Session: session, Content: p.content, Prompt: hitsPromptHash(),
			Model: m.Model, Effort: m.Effort, ModelID: modelID, JudgedAt: now, FirstEvent: p.first, LastEvent: p.last,
			Rules: shown, Hits: known(hits, p.judge), Partial: partial, Refused: refused,
		}
		if !observe {
			res.Hits.CostUSD = res.CostUSD
		}
		key := HitsKey(harnessName, session, p.content, m.mapCall(), shown)
		res.Hits.Fingerprint = strings.TrimSuffix(strings.TrimPrefix(key, hitsPrefix(harnessName, session)), ".json")
		if err := putJSON(ctx, m.Store, key, res.Hits); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// WouldCall reports whether mapping the session would call the model, and
// for what: to observe it, to judge it against rules, or both.
func (m *Mapper) WouldCall(ctx context.Context, harnessName, session string, evs []events.Event) (observe, judge bool, err error) {
	p, err := m.plan(ctx, harnessName, session, evs)
	if err != nil || !p.owner {
		return false, false, err
	}
	return p.cached == nil, len(p.judge) > 0, nil
}

// input builds one model input: the session, with the prompt for
// observations, rule hits, or both, and the rules to judge it against.
func (m *Mapper) input(rendered string, observe bool, judge []Rule) string {
	var b strings.Builder
	switch {
	case observe && len(judge) > 0:
		b.WriteString(mapPrompt + "\n\n" + hitsPrompt)
	case observe:
		b.WriteString(mapPrompt)
	default:
		b.WriteString(hitsOnlyPrompt + "\n\n" + hitsPrompt)
	}
	b.WriteString("\n\n# SESSION\n\n" + rendered)
	if len(judge) > 0 {
		b.WriteString("\n\n# CURRENT RULES\n\n")
		for _, r := range judge {
			fmt.Fprintf(&b, "Rule[%s]: %s\n", r.ID, r.Text)
		}
	}
	return b.String()
}

// call runs the map model over one input, adding to the usage and to the
// session's cost, and noting the model ID the harness reports.
func (m *Mapper) call(ctx context.Context, input string, cost *float64, modelID *string) (string, error) {
	out, usage, err := m.Runner.Run(ctx, m.mapCall(), input)
	m.mu.Lock()
	m.Usage = m.Usage.Add(usage)
	m.mu.Unlock()
	*cost += usage.CostUSD
	if usage.ModelID != "" {
		*modelID = usage.ModelID
	}
	return out, err
}

// salvage maps the longest prefix of a refused session the model accepts,
// counted in owner prompts. It starts below the session's own first
// refusal, if it has one, since what drew that refusal likely drew this
// one, then binary-searches down. A session with nothing salvageable is
// still cached, so later runs don't pay to be refused again; a new map
// model changes the fingerprint and retries it.
func (m *Mapper) salvage(ctx context.Context, evs []events.Event, judge []Rule, cost *float64, modelID *string) (string, *Partial, error) {
	var prompts []int // index in evs of each owner prompt
	firstRefusal := len(evs)
	for i, e := range evs {
		switch {
		case e.Kind == events.UserPrompt && !e.Sidechain:
			prompts = append(prompts, i)
		case e.Kind == events.Refusal && firstRefusal == len(evs):
			firstRefusal = i
		}
	}
	total := len(prompts)
	// prefix(k) is everything before owner prompt k: k prompts and their turns.
	prefix := func(k int) []events.Event {
		if k >= total {
			return evs
		}
		return evs[:prompts[k]]
	}
	// Prompts before the session's refusal, less the one that drew it.
	hi := total - 1
	if firstRefusal < len(evs) {
		before := 0
		for _, p := range prompts {
			if p < firstRefusal {
				before++
			}
		}
		hi = min(hi, before-1)
	}
	best, bestOut := 0, ""
	try := func(k int) (bool, error) {
		out, err := m.call(ctx, m.input(Render(prefix(k)), true, judge), cost, modelID)
		if errors.Is(err, model.ErrRefusal) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		best, bestOut = k, out
		return true, nil
	}
	if hi >= 1 {
		ok, err := try(hi)
		if err != nil {
			return "", nil, err
		}
		// Largest accepted k lies in [best, fail).
		lo, fail := 0, hi
		if ok {
			lo = hi
		}
		for fail-lo > 1 {
			mid := (lo + fail) / 2
			ok, err := try(mid)
			if err != nil {
				return "", nil, err
			}
			if ok {
				lo = mid
			} else {
				fail = mid
			}
		}
	}
	return bestOut, &Partial{Prompts: best, Of: total, Reason: "refusal"}, nil
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
	// deletes), so keep only the newest observation per host and session.
	// The host is part of the key because native memory notes reuse one
	// session key ("memory:<project>/<name>") on every machine, and one
	// machine's note must not shadow another's. Records written before
	// this change already carry their host, so they dedupe the same way;
	// an old record another host's note had shadowed simply reappears.
	newest := map[string]SessionObservations{}
	var order []string
	for _, key := range keys {
		so, ok, err := getJSON[SessionObservations](ctx, st, key)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // a torn or foreign object; skip, don't fail the window
		}
		id := so.Host + "\x00" + so.Session
		prev, seen := newest[id]
		if !seen {
			order = append(order, id)
		}
		if !seen || so.ObservedAt > prev.ObservedAt {
			newest[id] = so
		}
	}
	out := make([]SessionObservations, 0, len(order))
	for _, s := range order {
		out = append(out, newest[s])
	}
	return out, nil
}
