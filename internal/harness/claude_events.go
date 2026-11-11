package harness

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/micahhausler/recall/internal/events"
)

// line is one Claude Code transcript line; only metadata the mapping needs.
type line struct {
	Type               string          `json:"type"`
	Timestamp          string          `json:"timestamp"`
	CWD                string          `json:"cwd"`
	Entrypoint         string          `json:"entrypoint"`
	IsMeta             bool            `json:"isMeta"`
	IsSidechain        bool            `json:"isSidechain"`
	IsCompactSummary   bool            `json:"isCompactSummary"`
	RequestedModel     string          `json:"requestedModel"`
	APIRefusalCategory string          `json:"apiRefusalCategory"`
	Message            json.RawMessage `json:"message"`
}

type message struct {
	Role        string          `json:"role"`
	Model       string          `json:"model"`
	StopReason  string          `json:"stop_reason"`
	StopDetails *stopDetails    `json:"stop_details"`
	Content     json.RawMessage `json:"content"`
}

type stopDetails struct {
	Type     string `json:"type"`
	Category string `json:"category"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
}

var commandNameRE = regexp.MustCompile(`<command-name>\s*/?([^<\s]+)\s*</command-name>`)
var commandArgsRE = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)

// Sessions lists this harness's transcript files on disk.
func (c *Claude) Sessions() ([]string, error) {
	files, err := WalkFiles(c.source)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f.Rel, ".jsonl") {
			out = append(out, filepath.Join(filepath.Dir(c.source), filepath.FromSlash(f.Rel)))
		}
	}
	return out, nil
}

// ReadSessionFile reads one transcript from disk.
func (c *Claude) ReadSessionFile(path, host string) ([]events.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return c.ReadSession(f, strings.TrimSuffix(filepath.Base(path), ".jsonl"), host)
}

// ReadSession parses one transcript stream into normalized events.
// A malformed line is skipped: transcripts are append-only and an active
// session can have a torn last line.
func (c *Claude) ReadSession(r io.Reader, session, host string) ([]events.Event, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)
	var out []events.Event
	toolNames := map[string]string{} // tool_use id -> tool name
	base := func(l *line) events.Event {
		return events.Event{
			Harness:   c.Name(),
			Host:      host,
			Session:   session,
			CWD:       l.CWD,
			Timestamp: l.Timestamp,
			Sidechain: l.IsSidechain,
		}
	}
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		var msg message
		if len(l.Message) > 0 {
			if err := json.Unmarshal(l.Message, &msg); err != nil {
				continue
			}
		}
		switch l.Type {
		case "user":
			out = append(out, c.userEvents(&l, &msg, base, toolNames)...)
		case "assistant":
			out = append(out, c.assistantEvents(&l, &msg, base, toolNames)...)
		case "system":
			if l.APIRefusalCategory != "" {
				e := base(&l)
				e.Kind = events.Refusal
				e.Text = l.APIRefusalCategory
				out = append(out, e)
			}
		}
	}
	return out, sc.Err()
}

func (c *Claude) userEvents(l *line, msg *message, base func(*line) events.Event, toolNames map[string]string) []events.Event {
	if l.IsMeta {
		return nil // harness bookkeeping, not something a person said
	}
	if l.IsCompactSummary {
		e := base(l)
		e.Kind = events.Compact
		e.Role = "user"
		e.Text = contentText(msg.Content)
		return []events.Event{e}
	}
	var out []events.Event
	// Tool results come back as user turns.
	for _, b := range blocksOf(msg.Content) {
		if b.Type != "tool_result" {
			continue
		}
		e := base(l)
		e.Kind = events.ToolResult
		e.Role = "user"
		e.Tool = toolNames[b.ToolUseID]
		e.OK = events.Bool(!b.IsError)
		e.Text = contentText(b.Content)
		out = append(out, e)
	}
	text := contentText(msg.Content)
	if text == "" {
		return out
	}
	if m := commandNameRE.FindStringSubmatch(text); m != nil {
		e := base(l)
		e.Kind = events.SlashCommand
		e.Role = "user"
		e.Skill = m[1]
		if args := commandArgsRE.FindStringSubmatch(text); args != nil {
			e.Text = strings.TrimSpace(args[1])
		}
		return append(out, e)
	}
	if strings.HasPrefix(text, "<") {
		return out // command wrappers, local command output, reminders
	}
	if (orCLI(l.Entrypoint)) == "sdk-cli" {
		return out // a scripted `claude -p` run, not a typed prompt
	}
	e := base(l)
	e.Kind = events.UserPrompt
	e.Role = "user"
	e.Text = text
	return append(out, e)
}

func orCLI(entrypoint string) string {
	if entrypoint == "" {
		return "cli"
	}
	return entrypoint
}

func (c *Claude) assistantEvents(l *line, msg *message, base func(*line) events.Event, toolNames map[string]string) []events.Event {
	model := msg.Model
	if l.RequestedModel != "" {
		model = l.RequestedModel
	}
	var out []events.Event
	for _, b := range blocksOf(msg.Content) {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			e := base(l)
			e.Kind = events.AssistantText
			e.Role = "assistant"
			e.Model = model
			e.Text = b.Text
			out = append(out, e)
		case "tool_use":
			toolNames[b.ID] = b.Name
			e := base(l)
			e.Role = "assistant"
			e.Model = model
			e.Tool = b.Name
			if b.Name == "Skill" {
				e.Kind = events.SkillInvoked
				var in struct {
					Skill string `json:"skill"`
				}
				json.Unmarshal(b.Input, &in)
				e.Skill = in.Skill
			} else {
				e.Kind = events.ToolCall
			}
			out = append(out, e)
		}
	}
	if (msg.StopDetails != nil && msg.StopDetails.Type == "refusal") || msg.StopReason == "refusal" {
		e := base(l)
		e.Kind = events.Refusal
		e.Role = "assistant"
		e.Model = model
		if msg.StopDetails != nil {
			e.Text = msg.StopDetails.Category
		}
		out = append(out, e)
	}
	return out
}

// contentText joins the text blocks of a message content, which is either a
// plain string or a block list.
func contentText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var parts []string
	for _, b := range blocksOf(content) {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func blocksOf(content json.RawMessage) []block {
	var blocks []block
	if len(content) == 0 || json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	return blocks
}
