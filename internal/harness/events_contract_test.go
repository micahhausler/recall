package harness_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	eventsv1 "github.com/micahhausler/recall/api/events/v1"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/redact"
)

// Every event an adapter emits from its fixtures passes the contract's
// own check.
func TestAdapterEventsCheck(t *testing.T) {
	claude, _ := filepath.Glob(filepath.Join("testdata", "*.jsonl"))
	kiro, _ := filepath.Glob(filepath.Join("testdata", "kiro", "*.jsonl"))
	for name, paths := range map[string][]string{config.HarnessClaude: claude, config.HarnessKiro: kiro} {
		if len(paths) == 0 {
			t.Fatalf("no %s fixtures", name)
		}
		h, err := harness.Open(name, config.HarnessSettings{Source: t.TempDir(), Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			evs, err := h.ReadSession(f, "s", "laptop-a")
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			for i, e := range evs {
				if err := eventsv1.Check(e); err != nil {
					t.Errorf("%s event %d: %v", path, i+1, err)
				}
			}
		}
	}
}

// Summarize, redacted and trimmed, gives every Claude fixture's tool call
// the summary the adapter wrote before the contract held it.
func TestSummarizeMatchesClaudeFixtures(t *testing.T) {
	paths, _ := filepath.Glob(filepath.Join("testdata", "*.jsonl"))
	calls := 0
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 32<<20)
		for sc.Scan() {
			var l struct {
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(sc.Bytes(), &l) != nil {
				continue
			}
			var blocks []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(l.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type != "tool_use" {
					continue
				}
				calls++
				got, _ := redact.String(eventsv1.Summarize(b.Name, b.Input))
				if got, want := eventsv1.TrimInput(got), legacyToolInput(b.Name, b.Input); got != want {
					t.Errorf("%s %s: %q, want %q", path, b.Name, got, want)
				}
			}
		}
		f.Close()
	}
	if calls == 0 {
		t.Fatal("no tool calls in the fixtures")
	}
}

// legacyToolInput is the Claude adapter's summary as it was before
// eventsv1.Summarize.
func legacyToolInput(name string, raw json.RawMessage) string {
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	str := func(key string) string {
		var v string
		json.Unmarshal(in[key], &v)
		return v
	}
	var out string
	switch name {
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
			out = string(raw)
		}
	}
	out = strings.Join(strings.Fields(out), " ")
	out, _ = redact.String(out)
	if r := []rune(out); len(r) > 200 {
		out = string(r[:200]) + "…"
	}
	return out
}
