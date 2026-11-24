package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/config"
)

// The example plugin, built and named by path, imports through the real
// Exec into an fs store's machine; a second run changes nothing; and the
// stored sessions read back as the harness's events.
func TestImportExamplePluginEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the example plugin")
	}
	bin := filepath.Join(t.TempDir(), "import-example")
	build := exec.Command("go", "build", "-o", bin, "github.com/micahhausler/recall/examples/import-example")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	source, err := filepath.Abs(filepath.Join("..", "..", "examples", "import-example", "testdata", "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := newHookFixtureWith(t, "[harness.example]\nformat = \"events\"\n\n[harness.example.import]\n"+
		"command = [\""+bin+"\", \"--source\", \""+source+"\"]\ntimeout = \"1m\"\n")
	ctx := context.Background()
	out := captureStdout(t, func() {
		if code := runImports(ctx, f.env, []string{"example"}, importFlags{}, execSource); code != 0 {
			t.Errorf("import = %d", code)
		}
	})
	if !strings.Contains(out, "2 session(s) found, 2 stored, 0 unchanged, 1 filtered by the plugin") {
		t.Errorf("first run: %q", out)
	}
	sessions := filepath.Join(config.ImportDir(f.home, "example"), "sessions")
	for _, id := range []string{"s-0001", "chat-c-0001"} {
		got, err := os.ReadFile(filepath.Join(sessions, id+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("..", "..", "examples", "import-example", "testdata", "expected", id+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		// recall writes the host; everything else is the plugin's.
		if strings.ReplaceAll(string(got), `"host":"laptop-a",`, "") != string(want) {
			t.Errorf("%s stored as\n%s\nwant\n%s", id, got, want)
		}
	}
	old := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	os.Chtimes(filepath.Join(sessions, "s-0001.jsonl"), old, old)

	f.clk.T = f.clk.T.Add(24 * time.Hour)
	out = captureStdout(t, func() {
		if code := runImports(ctx, f.env, []string{"example"}, importFlags{}, execSource); code != 0 {
			t.Errorf("second import = %d", code)
		}
	})
	if !strings.Contains(out, "0 stored, 2 unchanged") {
		t.Errorf("second run: %q", out)
	}
	if st, _ := os.Stat(filepath.Join(sessions, "s-0001.jsonl")); !st.ModTime().Equal(old) {
		t.Error("an unchanged session was rewritten")
	}

	t.Setenv("HOME", f.home)
	t.Setenv("RECALL_CONFIG", "")
	out = captureStdout(t, func() {
		if code := cmdEvents([]string{"--harness", "example"}); code != 0 {
			t.Errorf("events = %d", code)
		}
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 19 || !strings.Contains(lines[0], `"harness":"example"`) || !strings.Contains(out, `"session":"chat-c-0001"`) {
		t.Errorf("recall events --harness example printed %d line(s):\n%s", len(lines), out)
	}
}
