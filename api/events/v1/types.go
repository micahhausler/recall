// Package v1 is the recall.events/v1 schema (docs/events.md): the
// normalized events every harness adapter emits and every import plugin
// writes. It imports only the standard library.
//
// The schema grows by optional fields and new kinds, never a rename or a
// removal.
package v1

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// APIVersion names this schema.
const APIVersion = "recall.events/v1"

// Kind is what happened in one event.
type Kind string

const (
	// UserPrompt is text a person typed into the harness's interface (on a
	// sidechain, the orchestrating agent "types" it; Sidechain says so).
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
	// BuiltinCommand is a slash command the harness ran itself, like /clear;
	// it is never a skill. Skill carries the command name.
	BuiltinCommand Kind = "builtin_command"
	// Refusal is the model or API declining to continue.
	Refusal Kind = "refusal"
	// Compact is a compaction summary standing in for earlier turns.
	Compact Kind = "compact"
)

// Kinds is every kind this version knows.
var Kinds = []Kind{UserPrompt, AssistantText, ToolCall, ToolResult, SkillInvoked,
	SlashCommand, BuiltinCommand, Refusal, Compact}

// Known reports whether k is a kind this version knows.
func (k Kind) Known() bool { return slices.Contains(Kinds, k) }

// Event is one normalized thing that happened in a session.
type Event struct {
	Harness string `json:"harness"`
	Host    string `json:"host,omitempty"`
	Session string `json:"session"`
	CWD     string `json:"cwd,omitempty"`
	// Repo is the origin of the repository the turn ran in, as host/path,
	// for a source that knows it without a checkout on this machine.
	Repo      string `json:"repo,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	// MessageID is the source's id for the message the event came from,
	// and ParentID the id of the message it follows; empty in a flat
	// session.
	MessageID string `json:"message_id,omitempty"`
	ParentID  string `json:"parent_id,omitempty"`
	Kind      Kind   `json:"kind"`
	Role      string `json:"role,omitempty"`
	Text      string `json:"text,omitempty"`
	Tool      string `json:"tool,omitempty"`
	// Input is a short summary of a tool call's input, such as a shell
	// command or a file path, with secret-shaped tokens redacted. A tool
	// result carries the input of the call it answers.
	Input string `json:"input,omitempty"`
	OK    *bool  `json:"ok,omitempty"`
	Skill string `json:"skill,omitempty"`
	// SkillDir is the directory the harness loaded a slash-invoked skill
	// from, when the transcript records it.
	SkillDir  string `json:"skill_dir,omitempty"`
	Model     string `json:"model,omitempty"`
	Sidechain bool   `json:"sidechain,omitempty"`
}

// Bool is a pointer to b, for the OK field.
func Bool(b bool) *bool { return &b }

// MaxIDLen bounds MessageID and ParentID.
const MaxIDLen = 128

// ErrUnknownKind is the error Check wraps for a kind this version doesn't
// know, which a reader drops and counts rather than rejecting the session.
var ErrUnknownKind = errors.New("unknown kind")

// Check validates one event on its own.
func Check(e Event) error {
	if !e.Kind.Known() {
		return fmt.Errorf("%w %q", ErrUnknownKind, e.Kind)
	}
	if e.Timestamp != "" {
		if _, err := time.Parse(time.RFC3339, e.Timestamp); err != nil {
			return fmt.Errorf("timestamp %q is not RFC 3339", e.Timestamp)
		}
	}
	switch e.Role {
	case "", "user", "assistant":
	default:
		return fmt.Errorf("role %q is not user or assistant", e.Role)
	}
	if e.OK != nil && e.Kind != ToolResult {
		return fmt.Errorf("ok is set on %s, not tool_result", e.Kind)
	}
	if len(e.MessageID) > MaxIDLen || len(e.ParentID) > MaxIDLen {
		return fmt.Errorf("message_id and parent_id are at most %d bytes", MaxIDLen)
	}
	if e.ParentID != "" && e.MessageID == "" {
		return errors.New("parent_id is set without message_id")
	}
	if e.ParentID != "" && e.ParentID == e.MessageID {
		return fmt.Errorf("message %q is its own parent", e.MessageID)
	}
	return nil
}

// CheckSession validates one session's events in order: each passes
// Check, all carry one session id, at least one is dated, and every
// parent_id names a message an earlier event came from.
func CheckSession(evs []Event) error {
	if len(evs) == 0 {
		return errors.New("no events")
	}
	seen := map[string]bool{}
	dated := false
	for i, e := range evs {
		if err := Check(e); err != nil {
			return fmt.Errorf("event %d: %w", i+1, err)
		}
		if e.Session != evs[0].Session {
			return fmt.Errorf("event %d: session %q, not %q", i+1, e.Session, evs[0].Session)
		}
		if e.ParentID != "" && !seen[e.ParentID] {
			return fmt.Errorf("event %d: parent_id %q names no earlier message", i+1, e.ParentID)
		}
		if e.MessageID != "" {
			seen[e.MessageID] = true
		}
		dated = dated || e.Timestamp != ""
	}
	if !dated {
		return errors.New("no event has a timestamp")
	}
	return nil
}

// InputLimit is how many characters of a tool's input an event keeps.
const InputLimit = 200

// TrimInput cuts s to InputLimit characters, marking a cut with "…".
func TrimInput(s string) string {
	if r := []rune(s); len(r) > InputLimit {
		return string(r[:InputLimit]) + "…"
	}
	return s
}

// Summarize writes a tool call's input on one line: the field that says
// what the call did, for the tools whose input has one, and compact JSON
// for the rest. It neither redacts nor cuts; recall does both before it
// stores an event.
func Summarize(tool string, input json.RawMessage) string {
	var in map[string]json.RawMessage
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	str := func(key string) string {
		var v string
		json.Unmarshal(in[key], &v)
		return v
	}
	var out string
	switch tool {
	case "Bash":
		out = str("command")
	case "Read", "Edit", "Write", "NotebookEdit":
		out = str("file_path")
		if out == "" {
			out = str("notebook_path")
		}
	case "Grep":
		out = str("pattern")
		if path := str("path"); path != "" {
			out += " in " + path
		}
	case "Glob":
		out = str("pattern")
	case "Agent", "Task":
		out = str("description")
	case "WebFetch":
		out = str("url")
	case "WebSearch":
		out = str("query")
	default:
		if len(in) > 0 {
			out = string(input)
		}
	}
	return strings.Join(strings.Fields(out), " ")
}
