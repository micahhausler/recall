package harness

import (
	"bufio"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/events"
)

// kiroLine is one transcript line in either Kiro layout: V2 lines carry
// kind and data, v3 lines carry timestamp and payload.
type kiroLine struct {
	// V2
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
	// v3
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// kiroV2Data is the data of a V2 Prompt, AssistantMessage, or ToolResults.
type kiroV2Data struct {
	Content []struct {
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	} `json:"content"`
	Meta struct {
		Timestamp int64 `json:"timestamp"`
	} `json:"meta"`
}

type kiroV2ToolUse struct {
	ToolUseID string `json:"toolUseId"`
	Name      string `json:"name"`
}

type kiroV2ToolResult struct {
	ToolUseID string `json:"toolUseId"`
	Status    string `json:"status"`
	Content   []struct {
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	} `json:"content"`
}

type kiroV2Thinking struct {
	ModelID string `json:"modelId"`
}

// kiroV3Payload is a v3 line's payload; only the fields the mapping needs.
type kiroV3Payload struct {
	Type             string          `json:"type"`
	Content          json.RawMessage `json:"content"`
	OperationType    string          `json:"operationType"`
	ReasoningModelID string          `json:"reasoningModelId"`
	ToolCallID       string          `json:"toolCallId"`
	ToolName         string          `json:"toolName"`
	Args             json.RawMessage `json:"args"`
	Success          *bool           `json:"success"`
	StopReason       string          `json:"stopReason"`
	Meta             struct {
		Kiro struct {
			SyntheticUserMessageReason string `json:"syntheticUserMessageReason"`
		} `json:"kiro"`
	} `json:"_meta"`
}

// kiroInjectionRE matches the bracketed tags Kiro and its add-ons put on
// prompts they write themselves, like [AGENT SYSTEM PROMPT] or [RUNTIME].
var kiroInjectionRE = regexp.MustCompile(`^\[[A-Z][A-Z _-]*\]`)

// kiroSteeringHeader introduces a message the person typed while a turn ran.
const kiroSteeringHeader = "[LIVE STEERING - New message from user]"

// kiroSkillPrefix marks a skill in a v3 load_resource call's id.
const kiroSkillPrefix = "skill::"

// kiroPromptText returns what a person typed in a user turn, or "" for a
// turn the harness or an add-on wrote.
func kiroPromptText(text string, synthetic bool) string {
	text = strings.TrimSpace(text)
	if rest, ok := strings.CutPrefix(text, kiroSteeringHeader); ok {
		return strings.TrimSpace(rest)
	}
	if synthetic || text == "" || strings.HasPrefix(text, "<") || kiroInjectionRE.MatchString(text) {
		return ""
	}
	return text
}

// ReadSession parses one Kiro transcript stream into normalized events.
// A malformed line is skipped, as an active session can have a torn last
// line.
func (k *Kiro) ReadSession(r io.Reader, session, host string) ([]events.Event, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)
	p := &kiroParser{session: session, host: host, toolNames: map[string]string{}, called: map[string]bool{}}
	for sc.Scan() {
		var l kiroLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		switch {
		case len(l.Payload) > 0:
			p.v3(&l)
		case l.Kind != "":
			p.v2(&l)
		}
	}
	return p.out, sc.Err()
}

type kiroParser struct {
	session, host string
	out           []events.Event
	toolNames     map[string]string // tool call id -> tool name
	called        map[string]bool   // v3 tool call ids already emitted
	timestamp     string            // V2: the last prompt's time, carried forward
	model         string            // the last model the transcript named
}

func (p *kiroParser) event(kind events.Kind, role, ts string) events.Event {
	return events.Event{Harness: "kiro", Host: p.host, Session: p.session, Timestamp: ts,
		Kind: kind, Role: role}
}

func (p *kiroParser) prompt(text, ts string) {
	e := p.event(events.UserPrompt, "user", ts)
	e.Text = text
	p.out = append(p.out, e)
}

func (p *kiroParser) v2(l *kiroLine) {
	var d kiroV2Data
	if json.Unmarshal(l.Data, &d) != nil {
		return
	}
	if d.Meta.Timestamp > 0 {
		p.timestamp = time.Unix(d.Meta.Timestamp, 0).UTC().Format(time.RFC3339)
	}
	ts := p.timestamp
	for _, c := range d.Content {
		var s string
		isText := c.Kind == "text" && json.Unmarshal(c.Data, &s) == nil
		switch {
		case (l.Kind == "Prompt" || l.Kind == "ToolResults") && isText:
			if text := kiroPromptText(s, l.Kind == "ToolResults" && !strings.HasPrefix(strings.TrimSpace(s), kiroSteeringHeader)); text != "" {
				p.prompt(text, ts)
			}
		case l.Kind == "AssistantMessage" && c.Kind == "thinking":
			var t kiroV2Thinking
			if json.Unmarshal(c.Data, &t) == nil && t.ModelID != "" {
				p.model = t.ModelID
			}
		case l.Kind == "AssistantMessage" && isText:
			if strings.TrimSpace(s) == "" {
				continue
			}
			e := p.event(events.AssistantText, "assistant", ts)
			e.Model, e.Text = p.model, s
			p.out = append(p.out, e)
		case l.Kind == "AssistantMessage" && c.Kind == "toolUse":
			var t kiroV2ToolUse
			if json.Unmarshal(c.Data, &t) != nil {
				continue
			}
			p.toolNames[t.ToolUseID] = t.Name
			e := p.event(events.ToolCall, "assistant", ts)
			e.Model, e.Tool = p.model, t.Name
			p.out = append(p.out, e)
		case l.Kind == "ToolResults" && c.Kind == "toolResult":
			var t kiroV2ToolResult
			if json.Unmarshal(c.Data, &t) != nil {
				continue
			}
			e := p.event(events.ToolResult, "user", ts)
			e.Tool = p.toolNames[t.ToolUseID]
			ok := t.Status == "success"
			var parts []string
			for _, rc := range t.Content {
				var text string
				if json.Unmarshal(rc.Data, &text) == nil {
					parts = append(parts, text)
					continue
				}
				var flag struct {
					IsError bool `json:"isError"`
				}
				if json.Unmarshal(rc.Data, &flag) == nil && flag.IsError {
					ok = false
				}
				parts = append(parts, string(rc.Data))
			}
			e.OK, e.Text = events.Bool(ok), strings.Join(parts, "\n")
			p.out = append(p.out, e)
		}
	}
}

func (p *kiroParser) v3(l *kiroLine) {
	var pl kiroV3Payload
	if json.Unmarshal(l.Payload, &pl) != nil {
		return
	}
	ts := l.Timestamp
	var text string
	json.Unmarshal(pl.Content, &text)
	switch pl.Type {
	case "user":
		if t := kiroPromptText(text, pl.Meta.Kiro.SyntheticUserMessageReason != ""); t != "" {
			p.prompt(t, ts)
		}
	case "assistant":
		if pl.ReasoningModelID != "" {
			p.model = pl.ReasoningModelID
		}
		if pl.OperationType != "Say" || strings.TrimSpace(text) == "" {
			return // reasoning, not prose
		}
		e := p.event(events.AssistantText, "assistant", ts)
		e.Model, e.Text = p.model, text
		p.out = append(p.out, e)
	case "tool_call":
		// A call is written again as its status moves (approved, completed).
		if p.called[pl.ToolCallID] {
			return
		}
		p.called[pl.ToolCallID] = true
		p.toolNames[pl.ToolCallID] = pl.ToolName
		e := p.event(events.ToolCall, "assistant", ts)
		e.Model, e.Tool = p.model, pl.ToolName
		if pl.ToolName == "load_resource" {
			var args struct {
				ID string `json:"id"`
			}
			json.Unmarshal(pl.Args, &args)
			if name, ok := strings.CutPrefix(args.ID, kiroSkillPrefix); ok {
				e.Kind, e.Skill = events.SkillInvoked, name
			}
		}
		p.out = append(p.out, e)
	case "tool_result":
		e := p.event(events.ToolResult, "user", ts)
		e.Tool = p.toolNames[pl.ToolCallID]
		e.OK = events.Bool(pl.Success == nil || *pl.Success)
		e.Text = text
		p.out = append(p.out, e)
	case "turn_end":
		if pl.StopReason == "refusal" {
			e := p.event(events.Refusal, "assistant", ts)
			e.Model = p.model
			p.out = append(p.out, e)
		}
	}
}
