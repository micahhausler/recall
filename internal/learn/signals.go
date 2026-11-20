package learn

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/redact"
)

// Signal is one thing an agent did at the start of a session that it may be
// doing at the start of every session: a file it opened, or a search it ran,
// to find its way around. The map model never sees these, since a
// successful tool call renders as its name alone; the binary counts them
// across sessions instead.
type Signal struct {
	Tool  string `json:"tool"`
	Input string `json:"input"`
}

// openingActions is how many of a session's first look-around actions are
// kept: enough to cover an agent orienting itself, not its whole task.
const openingActions = 15

// Shell commands that read the layout and change nothing: the ones that
// print files, whose arguments count as files read, and the ones that list
// or search, which count as the command they are.
var (
	printsFiles = map[string]bool{"cat": true, "head": true, "tail": true, "wc": true, "less": true, "bat": true}
	listsFiles  = map[string]bool{"ls": true, "find": true, "tree": true, "rg": true, "grep": true, "fd": true}
)

var shellSplitRE = regexp.MustCompile(`&&|\|\||[;|\n]`)

// lookArounds breaks a shell command line into its look-around parts. An
// agent often chains several in one call, and chains them differently each
// time, so the whole line never repeats; the files it prints and the
// directories it lists do. A file printed by cat is the same signal as one
// opened by the Read tool.
func lookArounds(command string) []Signal {
	var out []Signal
	for _, part := range shellSplitRE.Split(command, -1) {
		fields := strings.Fields(part)
		// Drop redirections, which say nothing about what was looked at.
		kept := fields[:0]
		for _, f := range fields {
			if !strings.ContainsAny(f, "<>") && f != "/dev/null" {
				kept = append(kept, f)
			}
		}
		if len(kept) == 0 {
			continue
		}
		switch {
		case printsFiles[kept[0]]:
			for _, arg := range kept[1:] {
				if strings.HasPrefix(arg, "-") || strings.Trim(arg, "0123456789") == "" {
					continue // a flag, or a flag's count
				}
				out = append(out, Signal{Tool: "Read", Input: strings.Trim(arg, `"'`)})
			}
		case listsFiles[kept[0]]:
			out = append(out, Signal{Tool: "Bash", Input: strings.Join(kept, " ")})
		}
	}
	return out
}

var homeDirRE = regexp.MustCompile(`/(?:home|Users)/[^/\s'"]+`)

// placeless rewrites a tool input so the same action in another worktree,
// or on another machine, reads the same: the session's own directory
// becomes ".", and a home directory "~", which also keeps the user name
// out of the store. A directory is stripped only on a path-separator
// boundary, so cwd /a/b never rewrites /a/bc or /a/b.txt.
func placeless(input, cwd, home string) string {
	if cwd != "" {
		input = strings.ReplaceAll(input, cwd+string(filepath.Separator), "")
		input = replaceDir(input, cwd, ".")
	}
	if home != "" {
		input = replaceDir(input, home, "~")
	}
	return homeDirRE.ReplaceAllString(input, "~")
}

// replaceDir rewrites dir to with inside input, only where the match ends
// the path: at the end of the input, or before a byte that can't continue a
// file name, such as a separator or a space.
func replaceDir(input, dir, with string) string {
	var b strings.Builder
	for {
		i := strings.Index(input, dir)
		if i < 0 {
			b.WriteString(input)
			return b.String()
		}
		end := i + len(dir)
		if end == len(input) || !nameByte(input[end]) {
			b.WriteString(input[:i])
			b.WriteString(with)
		} else {
			b.WriteString(input[:end])
		}
		input = input[end:]
	}
}

// nameByte reports whether b can continue a file name, so a directory's
// sibling that shares its prefix is never rewritten. Bytes outside ASCII
// are part of some multi-byte rune, most of them letters.
func nameByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
		b == '.' || b == '_' || b == '-' || b == '+' || b == '%' || b == '@' || b >= 0x80
}

// signalsOf returns the first look-around actions of a session's own turns:
// files read, by the Read tool or a shell command that prints them, and
// globs, greps, and listing or searching shell commands run.
func signalsOf(evs []events.Event, home string) []Signal {
	cwd := dominantCWD(evs)
	var out []Signal
	seen := map[Signal]bool{}
	for _, e := range evs {
		if len(out) == openingActions {
			break
		}
		if e.Kind != events.ToolCall || e.Sidechain || e.Input == "" {
			continue
		}
		var found []Signal
		switch e.Tool {
		case "Read", "Glob", "Grep":
			found = []Signal{{Tool: e.Tool, Input: e.Input}}
		case "Bash":
			found = lookArounds(e.Input)
		}
		for _, s := range found {
			s.Input = placeless(s.Input, cwd, home)
			// A signal is stored and repeated in observation text, so it
			// goes through the same redaction as a tool input summary.
			s.Input, _ = redact.String(s.Input)
			if len(out) < openingActions && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// failureTextLen is how much of a failure's normalized error text keys it.
const failureTextLen = 160

// failuresOf returns one Signal per distinct tool failure in a session: the
// tool and its error text, normalized the way look-around signals are, with
// paths made placeless and the text redacted and collapsed, so the same
// failure in another worktree or on another machine reads the same. Expected
// nonzero exits (benignFailure) don't count: a no-match grep recurring in
// every session is how searching works, not a lesson.
func failuresOf(evs []events.Event, home string) []Signal {
	cwd := dominantCWD(evs)
	var out []Signal
	seen := map[Signal]bool{}
	for _, e := range evs {
		if e.Kind != events.ToolResult || e.Sidechain || e.OK == nil || *e.OK {
			continue
		}
		if benignFailure(e.Tool, e.Input, e.Text) {
			continue
		}
		text := placeless(strings.TrimSpace(e.Text), cwd, home)
		text, _ = redact.String(text)
		text = strings.Join(strings.Fields(text), " ")
		if r := []rune(text); len(r) > failureTextLen {
			text = string(r[:failureTextLen])
		}
		if text == "" {
			continue
		}
		s := Signal{Tool: e.Tool, Input: text}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// repeatSessions is the fewest sessions that make a repeat: two can be one
// task split over two sittings.
const repeatSessions = 3

// maxRepeatActions is how many repeated actions one observation names.
const maxRepeatActions = 8

// CategoryRepeat marks an observation the binary counted from tool calls
// across sessions; no model reported it.
const CategoryRepeat = "repeat"

// SourceRepeat is a repeat observation's source: tool-derived, counted by
// the binary, never words the owner typed. With
// learn.repeat_evidence_sessions set, an add may rest on repeats alone.
const SourceRepeat = "repeat"

// withRepeats returns the window with one more observation on each session
// that took part in a repeat: a repo where at least repeatSessions
// sessions, and at least half of the sessions the owner typed into, began
// by opening the same files or running the same searches. That is an agent
// rediscovering a layout the repo's context file could have told it.
//
// With failureSessions above 0 (learn.repeat_evidence_sessions), the same
// tool failure, by tool and normalized error text, recurring in at least
// that many sessions is a repeat too, counted across every repo: a failure
// the agent recovers from each time can still deserve a rule when it keeps
// coming back. 0 counts no failures, so the key gates both the counting and
// what an add may rest on.
func withRepeats(window []SessionObservations, cutoff string, failureSessions int) []SessionObservations {
	type repo struct {
		sessions []int // indexes into window
		count    map[Signal]int
	}
	repos := map[string]*repo{}
	var keys []string
	for i, so := range window {
		key := so.Scope.Key()
		if key == "" || so.Prompts == 0 || so.Day() < cutoff {
			continue
		}
		r := repos[key]
		if r == nil {
			r = &repo{count: map[Signal]int{}}
			repos[key] = r
			keys = append(keys, key)
		}
		r.sessions = append(r.sessions, i)
		for _, s := range so.Signals {
			r.count[s]++
		}
	}
	sort.Strings(keys)
	out := append([]SessionObservations{}, window...)
	for _, key := range keys {
		r := repos[key]
		n := len(r.sessions)
		need := max(repeatSessions, (n+1)/2)
		var repeated []Signal
		for s, c := range r.count {
			if c >= need {
				repeated = append(repeated, s)
			}
		}
		if len(repeated) == 0 {
			continue
		}
		sort.Slice(repeated, func(i, j int) bool {
			a, b := repeated[i], repeated[j]
			if r.count[a] != r.count[b] {
				return r.count[a] > r.count[b]
			}
			return a.Tool+a.Input < b.Tool+b.Input
		})
		is := map[Signal]bool{}
		for _, s := range repeated {
			is[s] = true
		}
		if len(repeated) > maxRepeatActions {
			repeated = repeated[:maxRepeatActions]
		}
		var parts []string
		for _, s := range repeated {
			parts = append(parts, fmt.Sprintf("%s `%s` (%d of %d)", s.Tool, s.Input, r.count[s], n))
		}
		text := fmt.Sprintf("Sessions in %s keep starting with the same look around: %s.", key, strings.Join(parts, ", "))
		for _, i := range r.sessions {
			took := false
			for _, s := range out[i].Signals {
				took = took || is[s]
			}
			if took {
				// A fresh slice, so the caller's record isn't changed.
				obs := append([]Observation{}, out[i].Observations...)
				out[i].Observations = append(obs, Observation{Category: CategoryRepeat, Source: SourceRepeat, Text: text})
			}
		}
	}
	if failureSessions > 0 {
		need := max(failureSessions, 2) // one session is no repeat
		count := map[Signal][]int{}
		for i, so := range out {
			if so.Prompts == 0 || so.Day() < cutoff {
				continue
			}
			for _, f := range so.Failures {
				count[f] = append(count[f], i)
			}
		}
		var failures []Signal
		for f, sessions := range count {
			if len(sessions) >= need {
				failures = append(failures, f)
			}
		}
		sort.Slice(failures, func(i, j int) bool {
			a, b := failures[i], failures[j]
			if len(count[a]) != len(count[b]) {
				return len(count[a]) > len(count[b])
			}
			return a.Tool+a.Input < b.Tool+b.Input
		})
		for _, f := range failures {
			text := fmt.Sprintf("The same %s failure recurs in %d session(s): `%s`.", f.Tool, len(count[f]), f.Input)
			for _, i := range count[f] {
				obs := append([]Observation{}, out[i].Observations...)
				out[i].Observations = append(obs, Observation{Category: CategoryRepeat, Source: SourceRepeat, Text: text})
			}
		}
	}
	return out
}
