// Package events defines the one normalized event schema every harness
// adapter emits. The schema is documented in docs/events.md; adapters live
// with their harness.
package events

import (
	"bufio"
	"encoding/json"
	"io"
)

// Kind is what happened in one event.
type Kind string

const (
	// UserPrompt is a prompt a person typed into the interactive CLI
	// (on a sidechain, the orchestrating agent "types" it; Sidechain says so).
	UserPrompt Kind = "user_prompt"
	// AssistantText is prose the model wrote.
	AssistantText Kind = "assistant_text"
	// ToolCall is the model invoking a tool.
	ToolCall Kind = "tool_call"
	// ToolResult is a tool's answer; OK is false when the tool errored.
	ToolResult Kind = "tool_result"
	// SkillInvoked is a Skill tool call; Skill carries the skill name.
	SkillInvoked Kind = "skill_invoked"
	// SlashCommand is a user turn wrapped in <command-name>; Skill carries
	// the command name.
	SlashCommand Kind = "slash_command"
	// Refusal is the model or API declining to continue.
	Refusal Kind = "refusal"
	// Compact is a compaction summary standing in for earlier turns.
	Compact Kind = "compact"
)

// Event is one normalized thing that happened in a session.
type Event struct {
	Harness   string `json:"harness"`
	Host      string `json:"host,omitempty"`
	Session   string `json:"session"`
	CWD       string `json:"cwd,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Kind      Kind   `json:"kind"`
	Role      string `json:"role,omitempty"`
	Text      string `json:"text,omitempty"`
	Tool      string `json:"tool,omitempty"`
	OK        *bool  `json:"ok,omitempty"`
	Skill     string `json:"skill,omitempty"`
	Model     string `json:"model,omitempty"`
	Sidechain bool   `json:"sidechain,omitempty"`
}

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
func Bool(b bool) *bool { return &b }
