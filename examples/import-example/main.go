// Command import-example is an example recall import plugin
// (docs/import.md), built on api/imports/v1/command. It maps a synthetic
// source file in two common source shapes: an agent platform's
// flat turns, with one tool message holding a call and its result, and a
// chat product's conversations, whose messages form a tree.
//
// command.Run does the contract: it reads the request, checks every event
// and limit as the plugin writes it, and prints the answer. What is left
// here is fetch, where sessions come from, and the mapping.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	eventsv1 "github.com/micahhausler/recall/api/events/v1"
	importsv1 "github.com/micahhausler/recall/api/imports/v1"
	"github.com/micahhausler/recall/api/imports/v1/command"
)

const name = "import-example"

const usage = `import-example maps a synthetic source file into recall's events.

Usage:
  import-example --source PATH

recall sets the request in RECALL_IMPORT_INFO, runs this in an empty output
directory, and reads the answer from stdout.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv(importsv1.InfoEnv), os.Stdout, os.Stderr))
}

// run is the whole plugin, with everything main reads from the process
// passed in. It returns 0 for an answer, 1 for a failure, and 2 for a
// usage error.
func run(ctx context.Context, args []string, info string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	sourcePath := fs.String("source", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *sourcePath == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	fetch := func(ctx context.Context, req command.Request, out *command.Output) error {
		data, err := os.ReadFile(*sourcePath)
		if err != nil {
			return err
		}
		var src source
		if err := json.Unmarshal(data, &src); err != nil {
			return fmt.Errorf("%s: %w", *sourcePath, err)
		}
		for _, space := range src.Spaces {
			for _, s := range space.Sessions {
				if space.Desktop {
					// A desktop workspace's transcripts are already local.
					out.Filtered(1)
					continue
				}
				if err := write(out, s.ID, flatEvents(s)); err != nil {
					return err
				}
			}
		}
		for _, c := range src.Conversations {
			if err := write(out, "chat-"+c.UUID, treeEvents(c)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := command.Run(ctx, info, stdout, fetch); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}

func write(out *command.Output, id string, evs []eventsv1.Event) error {
	s, err := out.Session(id)
	if err != nil {
		return err
	}
	for _, e := range evs {
		if err := s.Add(e); err != nil {
			s.Close()
			return err
		}
	}
	return s.Close()
}

// source is the synthetic file: spaces of flat sessions, and conversations.
type source struct {
	Spaces        []space        `json:"spaces"`
	Conversations []conversation `json:"conversations"`
}

type space struct {
	Desktop  bool          `json:"desktop"`
	Sessions []flatSession `json:"sessions"`
}

type flatSession struct {
	ID        string        `json:"id"`
	Directory string        `json:"directory"`
	Messages  []flatMessage `json:"messages"`
}

// flatMessage is one turn: a prompt, a response, or a tool message whose
// content is a ToolCall as JSON.
type flatMessage struct {
	Type    string  `json:"type"`
	SentAt  float64 `json:"sentAt"`
	Content string  `json:"content"`
}

type toolCall struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Kind        string          `json:"kind"`
	Status      string          `json:"status"`
	RawInput    json.RawMessage `json:"rawInput"`
	RawOutput   json.RawMessage `json:"rawOutput"`
	StartedAt   int64           `json:"startedAt"`
	CompletedAt int64           `json:"completedAt"`
	Locations   []struct {
		Path string `json:"path"`
	} `json:"locations"`
}

// claudeTools are the tool names a title can start with.
var claudeTools = map[string]bool{"Read": true, "Write": true, "Edit": true, "Bash": true, "Grep": true, "Glob": true}

func seconds(s float64) string {
	return time.UnixMilli(int64(s * 1000)).UTC().Format(time.RFC3339)
}

func millis(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

// flatEvents maps one flat session, sorted by event time, since a tool
// message's call and result happen at its start and end.
func flatEvents(s flatSession) []eventsv1.Event {
	type timed struct {
		at time.Time
		e  eventsv1.Event
	}
	var out []timed
	add := func(ts string, e eventsv1.Event) {
		e.Timestamp, e.CWD = ts, s.Directory
		at, _ := time.Parse(time.RFC3339, ts)
		out = append(out, timed{at, e})
	}
	for _, m := range s.Messages {
		switch m.Type {
		case "INPUT":
			if strings.TrimSpace(m.Content) != "" {
				add(seconds(m.SentAt), eventsv1.Event{Kind: eventsv1.UserPrompt, Role: "user", Text: m.Content})
			}
		case "RESPONSE":
			add(seconds(m.SentAt), eventsv1.Event{Kind: eventsv1.AssistantText, Role: "assistant", Text: m.Content})
		case "TOOL":
			var c toolCall
			if json.Unmarshal([]byte(m.Content), &c) != nil {
				continue
			}
			var raw map[string]any
			json.Unmarshal(c.RawInput, &raw)
			if skill, ok := raw["skillName"].(string); ok && skill != "" {
				add(millis(c.StartedAt), eventsv1.Event{Kind: eventsv1.SkillInvoked, Role: "assistant", Skill: skill})
				continue
			}
			tool, input := toolOf(c, raw)
			add(millis(c.StartedAt), eventsv1.Event{Kind: eventsv1.ToolCall, Role: "assistant", Tool: tool, Input: input})
			add(millis(c.CompletedAt), eventsv1.Event{Kind: eventsv1.ToolResult, Role: "assistant", Tool: tool, Input: input,
				OK: eventsv1.Bool(c.Status != "failed"), Text: outputText(c.RawOutput)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	evs := make([]eventsv1.Event, len(out))
	for i, t := range out {
		evs[i] = t.e
	}
	return evs
}

// toolOf names a call's tool from its structured name, never its display
// title, and summarizes its input.
func toolOf(c toolCall, raw map[string]any) (tool, input string) {
	title := c.Title
	if title == "" {
		title = c.Name
	}
	ident, arg, _ := strings.Cut(title, " ")
	switch {
	case strings.HasPrefix(title, "Running: @"):
		tool = strings.TrimPrefix(title, "Running: ")
	case strings.HasPrefix(c.Name, "mcp__"):
		tool = c.Name
	case claudeTools[ident]:
		tool, input = ident, arg
	case c.Kind == "execute":
		tool = "shell"
	default:
		tool = c.Kind
	}
	for _, key := range []string{"command", "path", "query", "pattern"} {
		if v, ok := raw[key].(string); ok && v != "" {
			return tool, v
		}
	}
	switch {
	case input != "":
		return tool, input
	case len(raw) > 0:
		return tool, eventsv1.Summarize(tool, c.RawInput)
	case len(c.Locations) > 0 && c.Locations[0].Path != "":
		return tool, c.Locations[0].Path
	}
	rest := strings.TrimPrefix(title, "Running: ")
	if tool == "shell" {
		return tool, rest
	}
	_, arg, _ = strings.Cut(rest, " ")
	return tool, arg
}

// outputText flattens a tool's output: a string, a list of text parts, or
// items each holding Text or Json.
func outputText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var texts []string
		for _, p := range parts {
			texts = append(texts, p.Text)
		}
		return strings.Join(texts, "\n")
	}
	var items struct {
		Items []struct {
			Text string          `json:"Text"`
			JSON json.RawMessage `json:"Json"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &items) == nil {
		var texts []string
		for _, it := range items.Items {
			if it.Text != "" {
				texts = append(texts, it.Text)
			} else if len(it.JSON) > 0 {
				var b bytes.Buffer
				if json.Compact(&b, it.JSON) == nil {
					texts = append(texts, b.String())
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

type conversation struct {
	UUID     string        `json:"uuid"`
	Messages []chatMessage `json:"chat_messages"`
}

type chatMessage struct {
	UUID      string  `json:"uuid"`
	Sender    string  `json:"sender"`
	Parent    string  `json:"parent_message_uuid"`
	CreatedAt string  `json:"created_at"`
	Content   []block `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
	Start     string          `json:"start_timestamp"`
}

// rootSentinel is the parent a conversation's first messages name.
const rootSentinel = "00000000-0000-4000-8000-000000000000"

// treeEvents maps one conversation: every branch, parents before children,
// with each message's id and its parent's.
func treeEvents(c conversation) []eventsv1.Event {
	var evs []eventsv1.Event
	for _, m := range c.Messages {
		parent := m.Parent
		if parent == rootSentinel {
			parent = ""
		}
		base := eventsv1.Event{MessageID: m.UUID, ParentID: parent}
		at := func(b block) string {
			if b.Start != "" {
				return b.Start
			}
			return m.CreatedAt
		}
		if m.Sender == "human" {
			var texts []string
			for _, b := range m.Content {
				if t := typedText(b.Text); b.Type == "text" && strings.TrimSpace(t) != "" {
					texts = append(texts, t)
				}
			}
			if len(texts) > 0 {
				e := base
				e.Timestamp, e.Kind, e.Role, e.Text = m.CreatedAt, eventsv1.UserPrompt, "user", strings.Join(texts, "\n\n")
				evs = append(evs, e)
			}
			continue
		}
		calls := map[string]eventsv1.Event{}
		for _, b := range m.Content {
			e := base
			e.Timestamp, e.Role = at(b), "assistant"
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				e.Kind, e.Text = eventsv1.AssistantText, b.Text
			case "tool_use":
				if b.Name == "Skill" {
					var in struct {
						Skill string `json:"skill"`
					}
					json.Unmarshal(b.Input, &in)
					e.Kind, e.Skill = eventsv1.SkillInvoked, in.Skill
					break
				}
				e.Kind, e.Tool, e.Input = eventsv1.ToolCall, b.Name, eventsv1.Summarize(b.Name, b.Input)
				calls[b.ID] = e
			case "tool_result":
				call := calls[b.ToolUseID]
				e.Kind, e.Tool, e.Input = eventsv1.ToolResult, call.Tool, call.Input
				e.OK, e.Text = eventsv1.Bool(!b.IsError), outputText(b.Content)
			default:
				// thinking, flag, and anything else the product wrote.
				continue
			}
			evs = append(evs, e)
		}
	}
	return evs
}

// Markers around a comment the owner sent from an artifact.
const (
	commentHeader = "[Artifact comment sent to Claude]"
	commentBegin  = "=== BEGIN ARTIFACT COMMENT"
	commentEnd    = "=== END ARTIFACT COMMENT"
)

// typedText is what the owner typed in a human text block: the text
// itself, or for a comment sent from an artifact, the comment without the
// product's frame and its quote markers.
func typedText(text string) string {
	if !strings.HasPrefix(text, commentHeader) {
		return text
	}
	var lines []string
	in := false
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.HasPrefix(line, commentBegin):
			in = true
		case strings.HasPrefix(line, commentEnd):
			in = false
		case in:
			lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, ">"), " "))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
