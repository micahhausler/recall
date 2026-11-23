package learn

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/model"
)

//go:embed prompts/replay.md
var replayPrompt string

// A proposed rule exists because one moment went wrong, and that moment can
// be asked about again: the session up to the owner's correction, once with
// the target file as it is and once with the proposed rule added. A replay
// executes nothing; it is one tool-less call in which a model, reading the
// session as text, names the agent's next action and gives a structured
// verdict, and the binary, never the model, compares the two verdicts and
// decides what is held back.

// maxReplays is how many moments one reduce run may replay (learn.replay).
// A replay is at most two headless calls, so a full run spends at most 16,
// about what mapping 16 sessions costs, which keeps a weekly run's replay
// bill near the map's. A constant, not a config key: a reduce rarely
// proposes more adds than this, and a key would cost three documented
// places before anyone needs to tune it.
const maxReplays = 8

// SessionLoader returns one session's events as recorded on this machine;
// ok is false when the transcript is gone or was never here.
type SessionLoader func(harness, session string) ([]events.Event, bool)

// Replayer asks what the agent would do next at the moment that earned a
// proposal (learn.replay), given the session prefix up to the owner's
// correction, headless with no tools, like the reduce; nothing the session
// ran is run again. A replay that shows the rule changing nothing holds the
// add back; a replay that fails is logged and blocks nothing.
type Replayer struct {
	Runner model.Runner
	// Model is the model the replay runs as the agent, learn.map_model.
	Model string
	Load  SessionLoader
	// Cap bounds the moments replayed in one run; 0 means maxReplays.
	Cap int
	Out io.Writer
	// Usage accumulates across the run's replay calls.
	Usage model.Usage
	spent int
}

// ReplayRecord is one proposal's replay, written beside proposals.md so the
// apply reads a verdict no model can rewrite later.
type ReplayRecord struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	// Harness and Session name the replayed moment, and Model what played
	// the agent.
	Harness string `json:"harness,omitempty"`
	Session string `json:"session,omitempty"`
	Model   string `json:"model,omitempty"`
	// Without is the verdict with the target file as it is, With the verdict
	// with the proposed rule added; an ablation replay has only Without.
	Without string `json:"without,omitempty"`
	With    string `json:"with,omitempty"`
	// Held is why the apply holds the proposal back; empty lets it through.
	Held string `json:"held,omitempty"`
	// Error is a replay that didn't finish; it blocks nothing.
	Error string `json:"error,omitempty"`
}

const replayFile = "replay.json"

func saveReplays(dir string, recs []ReplayRecord) error {
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, replayFile), append(data, '\n'), 0o644)
}

// LoadReplays reads a reduce run's replay records; a run with no replays,
// such as one made with learn.replay off, has none.
func LoadReplays(dir string) ([]ReplayRecord, error) {
	data, err := os.ReadFile(filepath.Join(dir, replayFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []ReplayRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// replayHold says why a run's replays hold a proposal back, if they do.
func replayHold(recs []ReplayRecord, p Proposal) string {
	for _, rec := range recs {
		if rec.Kind == p.Kind && rec.Title == p.Title && rec.Held != "" {
			return rec.Held
		}
	}
	return ""
}

func (rp *Replayer) limit() int {
	if rp.Cap > 0 {
		return rp.Cap
	}
	return maxReplays
}

// take spends one of the run's replays, false when the cap is reached.
func (rp *Replayer) take() bool {
	if rp.spent >= rp.limit() {
		return false
	}
	rp.spent++
	return true
}

var verdictRE = regexp.MustCompile(`(?mi)^Verdict:\s*(repeats|avoids)\s*$`)

// parseVerdict reads the structured verdict a replay run must end with.
// Like the map's prefixes, the line is the contract: output in any other
// shape fails the replay rather than being judged by the binary.
func parseVerdict(out string) (string, error) {
	ms := verdictRE.FindAllStringSubmatch(out, -1)
	if len(ms) == 0 {
		return "", errors.New("the replay output has no Verdict line")
	}
	return strings.ToLower(ms[len(ms)-1][1]), nil
}

// run plays one arm of a replay and returns its verdict.
func (rp *Replayer) run(ctx context.Context, input string) (string, error) {
	out, usage, err := rp.Runner.Run(ctx, rp.Model, input)
	rp.Usage = rp.Usage.Add(usage)
	if err != nil {
		return "", err
	}
	return parseVerdict(out)
}

// replayInput builds one arm's model input: the rules the agent has loaded,
// the mistake to judge against, and the session up to the moment.
func replayInput(rendered, rules, mistake string) string {
	if strings.TrimSpace(rules) == "" {
		rules = "(none)"
	}
	return replayPrompt + "\n\n# RULES\n\n" + strings.TrimRight(rules, "\n") +
		"\n\n# MISTAKE\n\n" + mistake + "\n\n# SESSION\n\n" + rendered
}

// mistakeText describes the observed mistake for the replay: the
// observation and the owner's correction, quoted from the original session.
func mistakeText(text, quote string) string {
	out := strings.TrimSpace(text)
	if quote != "" {
		out += fmt.Sprintf("\nThe owner's correction, later in the original session: %q", quote)
	}
	return out
}

// momentPrefix returns the events up to and including the owner prompt the
// agent was answering when it made the mistake: everything before the
// prompt that holds the correction quote, so a replay re-decides that turn.
func momentPrefix(evs []events.Event, quote string) ([]events.Event, bool) {
	correction := -1
	for i, e := range evs {
		if e.Kind == events.UserPrompt && !e.Sidechain && ownerTyped(quote, []string{normalizeQuote(e.Text)}) {
			correction = i
			break
		}
	}
	for i := correction - 1; i >= 0; i-- {
		if evs[i].Kind == events.UserPrompt && !evs[i].Sidechain {
			return evs[:i+1], true
		}
	}
	return nil, false
}

// targetText is a target file's content as it is; a file that isn't here
// reads as empty.
func targetText(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// withoutBullet is a target's content with one rule's top-level bullet
// removed, by the same scan rules sync reads targets with, for an ablation
// run of the file without the rule.
func withoutBullet(content, text string) string {
	want := textHash(strings.TrimSpace(text))
	inCode := false
	var kept []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
		}
		if !inCode && line == trimmed {
			if m := bulletRE.FindStringSubmatch(trimmed); m != nil && textHash(strings.TrimSpace(m[1])) == want {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// squashRule is a proposal's rule text as one bullet line.
func squashRule(rule string) string {
	return strings.Join(strings.Fields(rule), " ")
}

// moment is one replayable point: a session still on this machine, the
// prefix up to the owner's correction, and the mistake it recorded.
type moment struct {
	harness, session string
	prefix           []events.Event
	text, quote      string
}

// momentOf finds the moment behind one piece of evidence, when its session
// is still on this machine and the quote anchors a prompt there.
func momentOf(ev Evidence, load SessionLoader) (moment, bool) {
	if ev.Source != SourceOwner || ev.Quote == "" || strings.HasPrefix(ev.Session, "memory:") {
		return moment{}, false
	}
	evs, ok := load(ev.Harness, ev.from())
	if !ok {
		return moment{}, false
	}
	prefix, ok := momentPrefix(evs, ev.Quote)
	if !ok {
		return moment{}, false
	}
	return moment{harness: ev.Harness, session: ev.from(), prefix: prefix, text: ev.Text, quote: ev.Quote}, true
}

// AddReplays replays, for each add not already held back, the moment that
// earned it: once with the target file as it is and once with the proposed
// rule added. An add whose replay shows the rule changing nothing is held
// back, with the reason in the record; a replay that fails blocks nothing.
func (rp *Replayer) AddReplays(ctx context.Context, proposals []Proposal, clusters []Cluster, flagged map[string]bool, home string) []ReplayRecord {
	byID := map[string]Cluster{}
	for _, k := range clusters {
		byID[k.ID] = k
	}
	var recs []ReplayRecord
	for _, p := range proposals {
		if p.Kind != "add" || flagged[p.Title] {
			continue
		}
		rec := ReplayRecord{Kind: p.Kind, Title: p.Title, Model: rp.Model}
		var m moment
		found := false
		for _, id := range p.Candidates {
			for _, ev := range byID[id].Evidence { // newest first
				if m, found = momentOf(ev, rp.Load); found {
					break
				}
			}
			if found {
				break
			}
		}
		switch {
		case !found:
			rec.Error = "no replayable moment: no owner-typed quote anchors a session still on this machine"
		case !rp.take():
			rec.Error = fmt.Sprintf("replay cap reached (%d per run)", rp.limit())
		default:
			rec.Harness, rec.Session = m.harness, m.session
			rendered := Render(m.prefix)
			rules := targetText(normTarget(p.Target, home))
			mistake := mistakeText(m.text, m.quote)
			var err error
			rec.Without, err = rp.run(ctx, replayInput(rendered, rules, mistake))
			if err == nil {
				withRule := strings.TrimRight(rules, "\n") + "\n- " + squashRule(p.Rule) + "\n"
				rec.With, err = rp.run(ctx, replayInput(rendered, withRule, mistake))
			}
			switch {
			case err != nil:
				rec.Error = err.Error()
			case rec.Without == "repeats" && rec.With == "avoids":
				// The rule turns the mistake into avoidance; the add stands.
			case rec.Without == rec.With:
				rec.Held = fmt.Sprintf("its replay shows no difference: the agent %s the mistake with and without the rule (learn.replay)", rec.Without)
			default:
				rec.Held = "its replay shows the agent avoiding the mistake only without the rule (learn.replay)"
			}
		}
		if rec.Error != "" && rp.Out != nil {
			fmt.Fprintf(rp.Out, "recall: replay %q: %s; not held back\n", p.Title, rec.Error)
		}
		recs = append(recs, rec)
	}
	return recs
}

// RecentModelDays is how far back "the models sessions run now" looks, for
// both the ablation's pick of outgrown rules and `recall learn age`.
const RecentModelDays = 30

// WindowModels are the models that answered in sessions of the last
// RecentModelDays days: what sessions run now, to the ablation and to
// `recall learn age`, which must share one cutoff.
func WindowModels(window []SessionObservations, now time.Time) []string {
	cutoff := now.AddDate(0, 0, -RecentModelDays).Format("2006-01-02")
	seen := map[string]bool{}
	var out []string
	for _, so := range window {
		if so.Day() < cutoff {
			continue
		}
		for _, m := range so.Models {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// ablationTitle names the retire an ablation proposes for a rule.
func ablationTitle(r Rule) string {
	return fmt.Sprintf("Retire %s: the current model follows it unprompted", r.ID)
}

// Ablation replays each outgrown rule's earning moments on the current
// model without the rule. A rule whose every replayed moment ends in
// `avoids` gets a retire block for the proposals, citing the replays; one
// moment that repeats the mistake, or a replay that can't run, proposes
// nothing, since ablation only ever removes on proof.
func (rp *Replayer) Ablation(ctx context.Context, outgrown []Rule, window []SessionObservations) (blocks []string, recs []ReplayRecord) {
	bySession := map[string]SessionObservations{}
	for _, so := range window {
		bySession[so.Session] = so
	}
	for _, rule := range outgrown {
		rec := ReplayRecord{Kind: "retire", Title: ablationTitle(rule), Model: rp.Model}
		var moments []moment
		for _, id := range rule.SourceSessions {
			so, ok := bySession[id]
			if !ok {
				continue
			}
			// The owner-typed observation most like the rule anchors the
			// moment the rule was written to prevent.
			best, bestSim := moment{}, -1.0
			for _, o := range so.Observations {
				ev := Evidence{Harness: so.Harness, Session: so.Session, Observation: o}
				if m, ok := momentOf(ev, rp.Load); ok {
					if sim := similarity(rule.Text, o.Text); sim > bestSim {
						best, bestSim = m, sim
					}
				}
			}
			if bestSim >= 0 {
				moments = append(moments, best)
			}
		}
		if len(moments) == 0 {
			rec.Error = "no replayable moment: no owner-typed quote anchors a session still on this machine"
			if rp.Out != nil {
				fmt.Fprintf(rp.Out, "recall: ablation %s: %s; nothing proposed\n", rule.ID, rec.Error)
			}
			recs = append(recs, rec)
			continue
		}
		rules := withoutBullet(targetText(rule.Target), rule.Text)
		avoided := true
		var sessions []string
		ruleRecs := []ReplayRecord{}
		for _, m := range moments {
			r := rec
			r.Harness, r.Session = m.harness, m.session
			if !rp.take() {
				r.Error = fmt.Sprintf("replay cap reached (%d per run)", rp.limit())
				avoided = false
			} else {
				verdict, err := rp.run(ctx, replayInput(Render(m.prefix), rules, mistakeText(m.text, m.quote)))
				if err != nil {
					r.Error = err.Error()
					avoided = false
				} else {
					r.Without = verdict
					avoided = avoided && verdict == "avoids"
				}
			}
			if r.Error != "" && rp.Out != nil {
				fmt.Fprintf(rp.Out, "recall: ablation %s: %s; nothing proposed\n", rule.ID, r.Error)
			}
			sessions = append(sessions, m.session)
			ruleRecs = append(ruleRecs, r)
			if !avoided {
				break // one miss already says the rule is still needed
			}
		}
		recs = append(recs, ruleRecs...)
		if !avoided {
			continue
		}
		blocks = append(blocks, fmt.Sprintf("### retire: %s\n- target: %s\n- replaces: %s\n- rule: %s\n- evidence: %s\n"+
			"- why: earned on %s, which no session in the last %d days ran; replayed without it on %s, the agent avoided the mistake in every replay (learn.replay)\n",
			ablationTitle(rule), rule.Target, rule.ID, squashRule(rule.Text), strings.Join(append([]string{rule.ID}, sessions...), ", "),
			strings.Join(rule.Models, ", "), RecentModelDays, rp.Model))
	}
	return blocks, recs
}
