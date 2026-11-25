package config

import (
	"path/filepath"
	"slices"
	"strings"
)

// The harnesses recall has an adapter for. Code that branches on a harness
// names it through these.
const (
	HarnessClaude = "claude"
	HarnessKiro   = "kiro"
)

// FormatEvents is the harness.<name>.format of a harness whose transcripts
// are recall's own normalized events, one session per file, such as an
// import's (docs/import.md).
const FormatEvents = "events"

// Formats are the values harness.<name>.format takes: a known harness's
// transcript format, or recall's own events.
var Formats = append(append([]string{}, KnownHarnesses...), FormatEvents)

// ImportDir is an events harness's state directory, which holds its
// stored sessions.
func ImportDir(home, name string) string {
	return filepath.Join(home, ".local", "state", "recall", "import", name)
}

// reservedHarnesses are store prefixes recall writes for itself, which a
// harness name, being a prefix too, can't take.
var reservedHarnesses = []string{"analysis", "hits", "reviews", "ledger", "learn"}

// reservedHarness reports whether name is one of recall's own prefixes.
func reservedHarness(name string) bool {
	return slices.Contains(reservedHarnesses, name) || strings.HasPrefix(name, "evidence-")
}

// harnessDefaults is what recall knows about one harness before any config
// file says otherwise. Paths are elements under the owner's home.
type harnessDefaults struct {
	name string
	// cli is the binary its runner and its inventory exec.
	cli string
	// source is where it keeps transcripts.
	source []string
	// target is the context file it loads into every session.
	target []string
	// skills is where it loads the owner's own skills from.
	skills []string
	// trailer is the commit trailer that marks its commits.
	trailer string
	// The models the loop asks its CLI for, by step.
	mapModel, reduceModel, applyModel string
	// efforts are the levels its CLI's --effort takes, lowest first.
	efforts []string
	// mapCost is about what one map call costs through its CLI on the
	// default map model, in costUnit, or in dollars when that is empty.
	mapCost  float64
	costUnit string
	// hookRunsLoop says its session-end hook can start the detached worker
	// that runs the loop. Without one, recur jobs run the loop instead.
	hookRunsLoop bool
	// loadsRepoNotes says its session-start hook adds the owner's notes for
	// the repo a session starts in (learn.repo_notes).
	loadsRepoNotes bool
}

// harnesses is every harness recall knows, in the order PrimaryHarness
// prefers them. A new harness is a constant, an entry here, an adapter
// listed under that constant in internal/harness/adapters, and a runner
// listed under it in internal/model/runners; cmd/recall's
// TestKnownHarnessesAreWired fails until all of them are there.
var harnesses = []harnessDefaults{
	{
		name:    HarnessClaude,
		cli:     "claude",
		source:  []string{".claude", "projects"},
		target:  []string{".claude", "CLAUDE.md"},
		skills:  []string{".claude", "skills"},
		trailer: "Co-Authored-By: Claude",
		// Measured on real sessions, haiku missed plain corrections that
		// sonnet caught, so the map runs on sonnet.
		mapModel:       "sonnet",
		reduceModel:    "opus",
		applyModel:     "sonnet",
		efforts:        []string{"low", "medium", "high", "xhigh", "max"},
		mapCost:        0.03,
		hookRunsLoop:   true,
		loadsRepoNotes: true,
	},
	{
		name:   HarnessKiro,
		cli:    "kiro-cli",
		source: []string{".kiro", "sessions"},
		target: []string{".kiro", "steering", "AGENTS.md"},
		skills: []string{".kiro", "skills"},
		// Kiro writes no trailer of its own, so its owner has it add this one.
		trailer: "Co-Authored-By: Kiro",
		// Kiro names its models differently; auto lets it choose.
		mapModel:    "auto",
		reduceModel: "auto",
		applyModel:  "auto",
		// GPT models also take none.
		efforts:  []string{"none", "low", "medium", "high", "xhigh", "max"},
		mapCost:  0.1,
		costUnit: "credits",
		// Kiro's session-end hook kills every process it starts, the
		// detached worker included. recall installs no session-start hook
		// for Kiro, so it loads no repo notes.
	},
}

// KnownHarnesses are the harnesses recall has an adapter for, in the order
// PrimaryHarness prefers them.
var KnownHarnesses = func() []string {
	names := make([]string, len(harnesses))
	for i, h := range harnesses {
		names[i] = h.name
	}
	return names
}()

// defaultsFor is the named harness's defaults; ok is false for one recall
// doesn't know.
func defaultsFor(name string) (harnessDefaults, bool) {
	for _, h := range harnesses {
		if h.name == name {
			return h, true
		}
	}
	return harnessDefaults{}, false
}

// Efforts are the effort levels the named runner takes, lowest first; a
// runner recall has no defaults for gets the first known harness's.
func Efforts(runner string) []string {
	h, ok := defaultsFor(runner)
	if !ok {
		h = harnesses[0]
	}
	return h.efforts
}

func under(home string, rel []string) string {
	if len(rel) == 0 {
		return ""
	}
	return filepath.Join(append([]string{home}, rel...)...)
}

// MapCost is about what one map call costs through the named runner's CLI,
// in unit, or in dollars when unit is empty; 0 for a runner recall has no
// figure for.
func MapCost(runner string) (per float64, unit string) {
	h, _ := defaultsFor(runner)
	return h.mapCost, h.costUnit
}

// PrimaryHarness is the enabled harness whose CLI and home the loop's
// defaults follow: the first known one that is on, else the first known one.
func PrimaryHarness(enabled map[string]HarnessSettings) string {
	for _, name := range KnownHarnesses {
		if _, ok := enabled[name]; ok {
			return name
		}
	}
	return KnownHarnesses[0]
}

// HookRunsLoop reports whether an enabled harness has a session-end hook
// that can run the loop. With none, recur jobs run it.
func HookRunsLoop(enabled map[string]HarnessSettings) bool {
	for _, h := range harnesses {
		if _, on := enabled[h.name]; on && h.hookRunsLoop {
			return true
		}
	}
	return false
}

// RepoNoteHarnesses are the enabled harnesses whose session-start hook
// loads repo notes, in table order.
func RepoNoteHarnesses(enabled map[string]HarnessSettings) []string {
	var out []string
	for _, h := range harnesses {
		if _, on := enabled[h.name]; on && h.loadsRepoNotes {
			out = append(out, h.name)
		}
	}
	return out
}

// Each Default below is "" for a harness recall doesn't know.

// DefaultCLI is the binary a known harness's runner and inventory exec.
func DefaultCLI(name string) string {
	h, _ := defaultsFor(name)
	return h.cli
}

// DefaultSkills is where a known harness loads the owner's own skills from,
// and so where a graduated skill lands.
func DefaultSkills(name, home string) string {
	h, _ := defaultsFor(name)
	return under(home, h.skills)
}

// DefaultTrailer is the commit trailer that marks a known harness's commits
// as agent-authored.
func DefaultTrailer(name string) string {
	h, _ := defaultsFor(name)
	return h.trailer
}

// DefaultSource is where a known harness keeps transcripts. A harness recall
// doesn't know must set harness.<name>.source, and harness.<name>.format to
// the known harness whose transcript format it shares.
func DefaultSource(name, home string) string {
	h, _ := defaultsFor(name)
	return under(home, h.source)
}

// DefaultTarget is the context file a known harness loads in every session
// and the owner edits: CLAUDE.md for Claude Code, the global AGENTS.md
// steering file for Kiro.
func DefaultTarget(name, home string) string {
	h, _ := defaultsFor(name)
	return under(home, h.target)
}
