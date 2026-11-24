package learn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/micahhausler/recall/internal/events"
)

// SourceOwner marks an observation whose quote the binary found, word for
// word, in a prompt the owner typed in that session. Everything else is
// what a model or a tool reported: tool output and assistant prose reach
// the map too, and a rule loaded into every session shouldn't rest on those
// alone.
const SourceOwner = "owner"

// normalizeQuote folds case, whitespace, and typographic quotes, so a quote
// matches the prompt it came from however either was wrapped.
func normalizeQuote(s string) string {
	s = strings.NewReplacer("‘", "'", "’", "'", "“", `"`, "”", `"`).Replace(s)
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// minQuoteWords is the shortest quote that counts, and minPieceWords the
// shortest piece an ellipsis may leave. A word or two, like "can you", is
// in most prompts and would mark any observation owner-typed, and a few
// scattered short pieces ("the ... and ... it") are in most prompts too.
const (
	minQuoteWords = 3
	minPieceWords = 3
)

// ownerTyped reports whether quote is in one of the owner's own prompts. A
// quote the model elided with an ellipsis counts when every piece is in the
// same prompt, in the quote's order, each on word boundaries and at least
// minPieceWords long. The check proves the owner typed those words, in that
// session; whether the observation follows from them is for the reviewer,
// who sees the quote beside the proposal.
func ownerTyped(quote string, prompts []string) bool {
	quote = strings.ReplaceAll(normalizeQuote(quote), "…", "...")
	var pieces []string
	words := 0
	for _, piece := range strings.Split(quote, "...") {
		if piece = strings.Trim(strings.TrimSpace(piece), `"'`); piece != "" {
			n := len(strings.Fields(piece))
			if n < minPieceWords {
				return false
			}
			pieces = append(pieces, piece)
			words += n
		}
	}
	if len(pieces) == 0 || words < minQuoteWords {
		return false
	}
	for _, prompt := range prompts {
		if containsInOrder(prompt, pieces) {
			return true
		}
	}
	return false
}

// containsInOrder reports whether every piece appears in s as whole words,
// in order, each at or after the end of the one before it.
func containsInOrder(s string, pieces []string) bool {
	at := 0
	for _, piece := range pieces {
		i := indexWords(s[at:], piece)
		if i < 0 {
			return false
		}
		at += i + len(piece)
	}
	return true
}

// indexWords is strings.Index on word boundaries: a match can't start or
// end inside a word, so "end" never matches inside "weekend".
func indexWords(s, sub string) int {
	for from := 0; ; from++ {
		i := strings.Index(s[from:], sub)
		if i < 0 {
			return -1
		}
		from += i
		if (from == 0 || !wordByte(s[from-1])) &&
			(from+len(sub) == len(s) || !wordByte(s[from+len(sub)])) {
			return from
		}
	}
}

// wordByte reports whether b can sit inside a word. Bytes outside ASCII are
// part of some multi-byte rune, most of them letters, so they count as word
// bytes; treating them as boundaries could match a piece inside one.
func wordByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

// verifyQuotes checks each observation's quote against the session's owner
// prompts. A quote found there marks the observation owner-typed. One that
// isn't found is removed: the model wrote it, and it isn't shown as the
// owner's words.
func verifyQuotes(obs []Observation, evs []events.Event) []Observation {
	var prompts []string
	for _, e := range evs {
		if e.Kind == events.UserPrompt && !e.Sidechain {
			prompts = append(prompts, normalizeQuote(e.Text))
		}
	}
	for i := range obs {
		obs[i].Source = ""
		if obs[i].Quote == "" {
			continue
		}
		if ownerTyped(obs[i].Quote, prompts) {
			obs[i].Source = SourceOwner
		} else {
			obs[i].Quote = ""
		}
	}
	return obs
}

// SessionRef is one session behind a candidate, as a reduce run recorded it.
type SessionRef struct {
	Session string `json:"session"`
	Host    string `json:"host,omitempty"`
	Day     string `json:"day"`
	// Repo is the session's Scope.Key, and Harness the harness it ran in;
	// outside evidence's is its source's, evidence-<name>.
	Repo    string `json:"repo,omitempty"`
	Harness string `json:"harness,omitempty"`
	// Owner reports whether the session holds an owner-typed observation
	// for this candidate, and Repeat whether it holds a repeat the binary
	// counted from tool calls.
	Owner  bool `json:"owner,omitempty"`
	Repeat bool `json:"repeat,omitempty"`
	// Models are the models that answered in the session.
	Models []string `json:"models,omitempty"`
}

// RunEvidence is what a reduce run knew about each candidate it showed,
// keyed by candidate id. It is written by the binary beside proposals.md,
// so the apply checks a proposal's evidence against facts no model wrote.
type RunEvidence map[string][]SessionRef

const evidenceFile = "evidence.json"

func runEvidence(clusters []Cluster) RunEvidence {
	out := RunEvidence{}
	for _, k := range clusters {
		at := map[string]int{}
		refs := []SessionRef{}
		for _, ev := range k.Evidence {
			i, ok := at[ev.from()]
			if !ok {
				i = len(refs)
				at[ev.from()] = i
				ref := SessionRef{Session: ev.from(), Host: ev.Host, Day: ev.Day, Repo: ev.Scope.Key(), Harness: ev.Harness}
				if ev.Origin == "" {
					ref.Models = ev.Models // a note's record names no model
				}
				refs = append(refs, ref)
			}
			refs[i].Owner = refs[i].Owner || ev.Source == SourceOwner
			refs[i].Repeat = refs[i].Repeat || ev.Source == SourceRepeat
		}
		out[k.ID] = refs
	}
	return out
}

func (e RunEvidence) save(dir string) error {
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, evidenceFile), append(data, '\n'), 0o644)
}

// LoadRunEvidence reads a reduce run's evidence record. A run from before
// there was one has none, which leaves every check that needs it unmet.
func LoadRunEvidence(dir string) (RunEvidence, error) {
	data, err := os.ReadFile(filepath.Join(dir, evidenceFile))
	if os.IsNotExist(err) {
		return RunEvidence{}, nil
	}
	if err != nil {
		return nil, err
	}
	var e RunEvidence
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	return e, nil
}

// sessions returns the sessions behind the candidates a proposal cites.
func (e RunEvidence) sessions(candidates []string) []SessionRef {
	var out []SessionRef
	for _, id := range candidates {
		out = append(out, e[id]...)
	}
	return out
}

// models returns the distinct models behind the candidates a proposal
// cites, sorted.
func (e RunEvidence) models(candidates []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range e.sessions(candidates) {
		for _, m := range ref.Models {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ownerTyped reports whether any cited candidate rests on words the owner
// typed.
func (e RunEvidence) ownerTyped(candidates []string) bool {
	for _, ref := range e.sessions(candidates) {
		if ref.Owner {
			return true
		}
	}
	return false
}

// repeatSessions counts the distinct sessions behind the cited candidates
// that hold a repeat observation: the same opening actions, counted there
// by the binary, not reported by a model or a tool.
func (e RunEvidence) repeatSessions(candidates []string) int {
	seen := map[string]bool{}
	for _, ref := range e.sessions(candidates) {
		if ref.Repeat {
			seen[ref.Host+"\x00"+ref.Session] = true
		}
	}
	return len(seen)
}

// needsOwnerEvidence reports whether a proposal puts something new in front
// of every session: a new rule, a new skill, or a graduate enforced by a
// hook, which runs code on every matching event. A reword, retire, or
// other graduate changes a rule the owner already reviewed.
func needsOwnerEvidence(p Proposal) bool {
	if p.Kind == "add" || (p.Kind == "skill" && p.Name != "") {
		return true
	}
	if p.Kind == "graduate" {
		e, why := parseEnforcement(p.Enforce, p.Hook, p.Permission)
		return why == "" && e.Kind == "hook"
	}
	return false
}

// supported reports whether a proposal that needs owner-typed evidence has
// it. An add may instead rest on repeats the binary counted in at least
// minRepeats sessions (learn.repeat_evidence_sessions); 0, the default,
// allows no such add, and nothing else ever rests on repeats.
func supported(p Proposal, e RunEvidence, minRepeats int) bool {
	if e.ownerTyped(p.Candidates) {
		return true
	}
	return p.Kind == "add" && minRepeats > 0 && e.repeatSessions(p.Candidates) >= minRepeats
}

// Unsupported returns the proposals that need owner-typed evidence and
// cite none.
func Unsupported(proposals []Proposal, e RunEvidence, minRepeats int) []Proposal {
	var out []Proposal
	for _, p := range proposals {
		if needsOwnerEvidence(p) && !supported(p, e, minRepeats) {
			out = append(out, p)
		}
	}
	return out
}
