// Package learn is the loop that replaces distill (docs/learning.md): a
// cheap cached map pass writes structured observations per session, and a
// reduce pass compares them with the current rules and proposes edits.
package learn

import (
	"fmt"
	"regexp"
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
// calls to [tool: name], failures keep a short tail of their output, and
// owner messages stay whole.
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
			if e.OK != nil && !*e.OK {
				tail := strings.TrimSpace(e.Text)
				if len(tail) > 200 {
					tail = "…" + tail[len(tail)-200:]
				}
				fmt.Fprintf(&b, "[tool %s FAILED: %s]\n", e.Tool, tail)
			}
		case events.SkillInvoked:
			fmt.Fprintf(&b, "[skill: %s]\n", e.Skill)
		case events.SlashCommand:
			fmt.Fprintf(&b, "[command: /%s %s]\n", e.Skill, strings.TrimSpace(e.Text))
		case events.Refusal:
			fmt.Fprintf(&b, "[refusal: %s]\n", e.Text)
		case events.Compact:
			b.WriteString("[earlier turns compacted]\n")
		}
	}
	return b.String()
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
