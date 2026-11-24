// Package events is recall's side of the normalized event schema,
// api/events/v1 (docs/events.md); adapters live with their harness.
package events

import (
	"bufio"
	"encoding/json"
	"io"

	eventsv1 "github.com/micahhausler/recall/api/events/v1"
)

// Kind is what happened in one event.
type Kind = eventsv1.Kind

// Event is one normalized thing that happened in a session.
type Event = eventsv1.Event

const (
	UserPrompt     = eventsv1.UserPrompt
	AssistantText  = eventsv1.AssistantText
	ToolCall       = eventsv1.ToolCall
	ToolResult     = eventsv1.ToolResult
	SkillInvoked   = eventsv1.SkillInvoked
	SlashCommand   = eventsv1.SlashCommand
	BuiltinCommand = eventsv1.BuiltinCommand
	Refusal        = eventsv1.Refusal
	Compact        = eventsv1.Compact
)

// WriteJSONL writes events one JSON object per line.
func WriteJSONL(w io.Writer, evs []Event) error {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for _, e := range evs {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// Bool is a pointer to b, for the OK field.
func Bool(b bool) *bool { return eventsv1.Bool(b) }
