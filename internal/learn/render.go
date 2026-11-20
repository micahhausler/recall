// Package learn is the loop that replaces distill (docs/learning.md): a
// cheap cached map pass writes structured observations per session, and a
// reduce pass compares them with the current rules and proposes edits.
package learn

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/micahhausler/recall/internal/events"
)

// assistantBudget is how much of an assistant message survives compression.
// Owner messages are preserved in full; assistant prose is mostly context.
const assistantBudget = 500

// correctionWindow: when the owner's next message is a short correction,
// the preceding assistant text is likely what prompted it, so more of it is
// preserved (muse's context-aware compression).
const (
	correctionMaxWords  = 50
	correctionAssistant = 2000
)

var codeBlockRE = regexp.MustCompile("(?s)```.*?```")

// Render compresses one session's events into the text the map model reads.
// Mechanical, not model-driven: code blocks collapse to [code block], tool
// calls to [tool: name], failures keep what was run and a short tail of its
// output, and owner messages stay whole.
func Render(evs []events.Event) string {
	var b strings.Builder
	for i, e := range evs {
		if e.Sidechain {
			continue // subagent chatter; the main thread carries the owner
		}
		switch e.Kind {
		case events.UserPrompt:
			fmt.Fprintf(&b, "[owner] %s\n\n", strings.TrimSpace(e.Text))
		case events.AssistantText:
			text := codeBlockRE.ReplaceAllString(e.Text, "[code block]")
			budget := assistantBudget
			if isShortCorrection(evs, i) {
				budget = correctionAssistant
			}
			if len(text) > budget {
				text = text[:budget] + " […]"
			}
			fmt.Fprintf(&b, "[assistant] %s\n\n", strings.TrimSpace(text))
		case events.ToolCall:
			fmt.Fprintf(&b, "[tool: %s]\n", e.Tool)
		case events.ToolResult:
			if e.OK != nil && !*e.OK && !benignFailure(e.Tool, e.Input, e.Text) {
				tail := strings.TrimSpace(e.Text)
				if len(tail) > 200 {
					tail = "…" + tail[len(tail)-200:]
				}
				if e.Input != "" {
					fmt.Fprintf(&b, "[tool %s FAILED on `%s`: %s]\n", e.Tool, e.Input, tail)
				} else {
					fmt.Fprintf(&b, "[tool %s FAILED: %s]\n", e.Tool, tail)
				}
			}
		case events.SkillInvoked:
			fmt.Fprintf(&b, "[skill: %s]\n", e.Skill)
		case events.SlashCommand, events.BuiltinCommand:
			fmt.Fprintf(&b, "[command: /%s %s]\n", e.Skill, strings.TrimSpace(e.Text))
		case events.Refusal:
			fmt.Fprintf(&b, "[refusal: %s]\n", e.Text)
		case events.Compact:
			b.WriteString("[earlier turns compacted]\n")
		}
	}
	return b.String()
}

// Shell commands whose nonzero exit is an answer, not a mistake: grep and rg
// exit 1 on no match, diff exits 1 when the files differ, and test (or [)
// exits 1 for false.
var benignExits = map[string]bool{
	"grep": true, "egrep": true, "fgrep": true, "rg": true,
	"diff": true, "test": true, "[": true,
}

// exitCodeRE matches the "Exit code N" line a harness puts before a failed
// shell command's output.
var exitCodeRE = regexp.MustCompile(`^Exit code (\d+)\n?`)

// benignFailure reports whether a failed tool result is an expected nonzero
// exit rather than something going wrong, judged from the command and its
// output. A chain's or pipeline's status is its last command's, so that
// command decides; exit codes above 1 are real errors (grep's bad pattern,
// diff's unreadable file) and still render FAILED. A no-match grep and a
// false test print nothing, so any output beyond the exit-code line means
// something else in the chain went wrong; diff's differences are its output.
func benignFailure(tool, command, output string) bool {
	if tool != "Bash" || command == "" {
		return false
	}
	code := -1
	if m := exitCodeRE.FindStringSubmatch(output); m != nil {
		code, _ = strconv.Atoi(m[1])
		output = output[len(m[0]):]
	}
	if code > 1 {
		return false
	}
	parts := shellSplitRE.Split(command, -1)
	fields := strings.Fields(parts[len(parts)-1])
	if len(fields) == 0 {
		return false
	}
	cmd := fields[0]
	if i := strings.LastIndexByte(cmd, '/'); i >= 0 {
		cmd = cmd[i+1:] // /usr/bin/grep is grep
	}
	if !benignExits[cmd] {
		return false
	}
	if cmd == "diff" {
		// Differences found is exit 1 exactly; an unknown code could as well
		// be diff's exit 2, trouble.
		return code == 1
	}
	return strings.TrimSpace(output) == ""
}

// isShortCorrection reports whether the next owner message after i is a
// short correction (under correctionMaxWords words).
func isShortCorrection(evs []events.Event, i int) bool {
	for _, e := range evs[i+1:] {
		if e.Sidechain {
			continue
		}
		switch e.Kind {
		case events.UserPrompt:
			return len(strings.Fields(e.Text)) <= correctionMaxWords
		case events.AssistantText, events.Compact:
			return false
		}
	}
	return false
}
