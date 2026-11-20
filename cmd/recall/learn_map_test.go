package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr runs fn with os.Stderr swapped for a pipe and returns what
// it printed.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// undatedTranscript writes one claude session whose lines carry no
// timestamps, the way a hand-built or damaged transcript can.
func undatedTranscript(t *testing.T, f *hookFixture, session string) {
	t.Helper()
	dir := filepath.Join(f.home, ".claude", "projects", "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := fmt.Sprintf(`{"type":"user","cwd":%q,"entrypoint":"cli","message":{"role":"user","content":"always run go vet before saying a change is done"}}
{"type":"assistant","message":{"model":"claude-sonnet-4-5","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}}
`, filepath.Join(f.home, "w"))
	if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --since all reads everything, including a session whose events carry no
// timestamps: it maps as undated instead of falling out of every window.
func TestMapSinceAllIncludesTimestamplessSessions(t *testing.T) {
	f := newHookFixture(t)
	undatedTranscript(t, f, "sess-undated")

	out := captureStdout(t, func() {
		if code := learnMap(context.Background(), f.env, f.runner, "all", 0, false, false, 0, nil); code != 0 {
			t.Errorf("learn map --since all = %d", code)
		}
	})
	mapped := false
	for _, in := range f.runner.calls {
		if strings.Contains(in, "always run go vet") {
			mapped = true
		}
	}
	if !mapped {
		t.Errorf("--since all never mapped the timestampless session:\n%s", out)
	}
}

// A bounded window can't place a session with no timestamps, so the map
// skips it, and says so once, naming the --since all remedy, instead of
// passing over it in silence.
func TestMapBoundedWindowNamesSkippedTimestamplessSessions(t *testing.T) {
	f := newHookFixture(t)
	undatedTranscript(t, f, "sess-undated")

	errOut := captureStderr(t, func() {
		if code := learnMap(context.Background(), f.env, f.runner, "30d", 0, false, false, 0, nil); code != 0 {
			t.Errorf("learn map = %d", code)
		}
	})
	for _, in := range f.runner.calls {
		if strings.Contains(in, "always run go vet") {
			t.Error("a bounded window mapped the timestampless session")
		}
	}
	if !strings.Contains(errOut, "1 session(s)") || !strings.Contains(errOut, "--since all") {
		t.Errorf("the skip was never named with its remedy:\n%s", errOut)
	}
}
