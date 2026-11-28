package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/learn"
)

// An events harness's sessions ran on another machine, so the map never
// looks their paths up here, even when a checkout here has the same path.
func TestMapEventsHarnessSkipsCheckoutLookup(t *testing.T) {
	f := newHookFixtureWith(t, "[learn]\nreview = \"branch\"\n\n[harness.chat]\nformat = \"events\"\n").noOwnerEvidence()
	repo := filepath.Join(f.home, "src", "example")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@example.com:you/example.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := f.clk.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	sessions := filepath.Join(config.ImportDir(f.home, "chat"), "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := fmt.Sprintf(`{"harness":"chat","session":"s-0001","cwd":%q,"timestamp":%q,"kind":"user_prompt","role":"user","text":"always run go vet before saying a change is done"}
{"harness":"chat","session":"s-0001","cwd":%q,"timestamp":%q,"kind":"assistant_text","role":"assistant","text":"ok"}
`, repo, ts, repo, ts)
	if err := os.WriteFile(filepath.Join(sessions, "s-0001.jsonl"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	captureStdout(t, func() {
		if code := learnMap(context.Background(), f.env, f.runner, "30d", 0, false, false, 0, nil); code != 0 {
			t.Errorf("learn map = %d", code)
		}
	})
	ctx := context.Background()
	st, _, err := openStore(ctx, f.env)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := st.List(ctx, "analysis/chat/")
	if err != nil || len(keys) != 1 || !strings.Contains(keys[0], "s-0001") {
		t.Fatalf("observations = %v, %v", keys, err)
	}
	r, err := st.Get(ctx, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var so learn.SessionObservations
	if err := json.NewDecoder(r).Decode(&so); err != nil {
		t.Fatal(err)
	}
	if so.Harness != "chat" || so.Scope.Repo != "" || so.Scope.Remote != "" || so.Scope.CWD == "" {
		t.Errorf("harness %q, scope %+v; want the cwd kept and no checkout", so.Harness, so.Scope)
	}

	// The reduce reads the observations of a harness its config declares
	// with the format line alone, no import table.
	_, window, err := loadLedgerAndHits(ctx, f.env, st, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range window {
		found = found || (w.Harness == "chat" && w.Session == "s-0001")
	}
	if !found {
		t.Errorf("the reduce's window has no chat observation: %d observation(s)", len(window))
	}
}
