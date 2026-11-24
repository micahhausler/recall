package learn

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/harness"
)

var updateRender = flag.Bool("update-render", false, "rewrite testdata/render from the adapter fixtures")

// A flat session renders byte for byte as the goldens, so no stored
// fingerprint moves.
func TestRenderAdapterFixtures(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("..", "harness", "testdata", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	kiro, _ := filepath.Glob(filepath.Join("..", "harness", "testdata", "kiro", "*.jsonl"))
	fixtures = append(fixtures, kiro...)
	if len(fixtures) == 0 {
		t.Fatal("no fixtures")
	}
	for _, path := range fixtures {
		name := config.HarnessClaude
		if filepath.Base(filepath.Dir(path)) == "kiro" {
			name = config.HarnessKiro
		}
		h, err := harness.Open(name, config.HarnessSettings{Source: t.TempDir(), Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		evs, err := h.ReadSession(f, "s", "laptop-a")
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		golden := filepath.Join("testdata", "render", name+"-"+strings.TrimSuffix(filepath.Base(path), ".jsonl")+".txt")
		got := Render(evs)
		if *updateRender {
			os.MkdirAll(filepath.Dir(golden), 0o755)
			if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s renders differently from %s:\n%s", path, golden, got)
		}
	}
}

func treeEvent(kind events.Kind, msg, parent, text string) events.Event {
	e := events.Event{Harness: "chat", Session: "s", Timestamp: "2026-03-02T12:00:00Z",
		MessageID: msg, ParentID: parent, Kind: kind, Text: text}
	switch kind {
	case events.UserPrompt:
		e.Role = "user"
	default:
		e.Role = "assistant"
	}
	return e
}

func TestRenderTree(t *testing.T) {
	evs := []events.Event{
		treeEvent(events.UserPrompt, "u1", "", "first prompt"),
		treeEvent(events.AssistantText, "a1", "u1", "first answer"),
		treeEvent(events.UserPrompt, "u2", "a1", "follow up"),
		treeEvent(events.AssistantText, "a2", "u2", "old answer"),
		treeEvent(events.AssistantText, "a3", "u2", "new answer"),
		treeEvent(events.UserPrompt, "u3", "a1", "follow up, reworded"),
		treeEvent(events.AssistantText, "a4", "u3", "answer to the edit"),
	}
	want := "[owner] first prompt\n\n" +
		"[assistant] first answer\n\n" +
		markAbandoned +
		"[owner] follow up\n\n" +
		markAbandoned +
		"[assistant] old answer\n\n" +
		markAbandonEnd +
		markRegenerated +
		"[assistant] new answer\n\n" +
		markAbandonEnd +
		markEdited +
		"[owner] follow up, reworded\n\n" +
		"[assistant] answer to the edit\n\n"
	if got := Render(evs); got != want {
		t.Errorf("Render =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderTreeFileOrder(t *testing.T) {
	// The edit comes before the abandoned branch's answer in the file; the
	// render still keeps each branch whole.
	evs := []events.Event{
		treeEvent(events.UserPrompt, "u1", "", "draft"),
		treeEvent(events.UserPrompt, "u2", "", "edited"),
		treeEvent(events.AssistantText, "a1", "u1", "reply to draft"),
		treeEvent(events.AssistantText, "a2", "u2", "reply to edit"),
	}
	want := markAbandoned +
		"[owner] draft\n\n" +
		"[assistant] reply to draft\n\n" +
		markAbandonEnd +
		markEdited +
		"[owner] edited\n\n" +
		"[assistant] reply to edit\n\n"
	if got := Render(evs); got != want {
		t.Errorf("Render =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderTreeWithoutFork(t *testing.T) {
	flat := []events.Event{
		treeEvent(events.UserPrompt, "", "", "short fix"),
		treeEvent(events.AssistantText, "", "", "done"),
	}
	tree := []events.Event{
		treeEvent(events.UserPrompt, "u1", "", "short fix"),
		treeEvent(events.AssistantText, "a1", "u1", "done"),
	}
	if got, want := Render(tree), Render(flat); got != want {
		t.Errorf("tree with no fork = %q, want %q", got, want)
	}
}
