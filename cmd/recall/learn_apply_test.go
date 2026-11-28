package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/learn"
)

// captureStdout runs fn with os.Stdout swapped for a pipe and returns what
// it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
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

// A graduate the plan announced can still be held back inside Run, by the
// settings pre-check on the staged copy of learn.settings. That hold must
// reach stdout as a "not applied" line with its reason, not vanish between
// the plan's skips and the outcome report.
func TestApplyPrintsGraduateHeldByStagedSettings(t *testing.T) {
	f := newHookFixtureWith(t, "[learn]\nreview = \"branch\"\n"+
		"targets = [\"~/.claude/CLAUDE.md\"]\nsettings = \"~/.claude/settings.json\"\n").noOwnerEvidence()
	ctx := context.Background()
	dot := filepath.Join(f.home, ".claude")

	// The committed settings file is a JSON array, so the staged copy Run
	// checks can't take a permission rule; the live copy is a valid object,
	// so the plan lets the graduate through.
	if err := os.WriteFile(filepath.Join(dot, "CLAUDE.md"), []byte("# Rules\n\n- Ask before a push.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dot, "settings.json")
	if err := os.WriteFile(settings, []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dot, "add", "-A")
	gitIn(t, dot, "commit", "-q", "-m", "settings")
	if err := os.WriteFile(settings, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := learnRulesSync(ctx, f.env); code != 0 {
		t.Fatalf("rules sync = %d", code)
	}
	ledger, err := learn.LoadLedger(config.LedgerPath(f.env.home))
	if err != nil || len(ledger.Rules) != 1 {
		t.Fatalf("ledger after sync: %+v, %v", ledger, err)
	}

	run := filepath.Join(learnStateRoot(f.env), "2026-11-18")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	proposals := "## Proposals\n\n### graduate: Ask before a push\n- replaces: " + ledger.Rules[0].ID + "\n" +
		"- enforce: permission\n- permission: ask Bash(git push:*)\n- rule: no script\n- evidence: s-1\n"
	if err := os.WriteFile(filepath.Join(run, "proposals.md"), []byte(proposals), 0o644); err != nil {
		t.Fatal(err)
	}

	code := -1
	out := captureStdout(t, func() { code, _ = applyRun(ctx, f.env, run, false) })
	if code != 0 {
		t.Fatalf("applyRun = %d\n%s", code, out)
	}
	if !strings.Contains(out, "apply graduate: Ask before a push") {
		t.Fatalf("the plan never announced the graduate:\n%s", out)
	}
	if !strings.Contains(out, "not applied graduate: Ask before a push: the binary can't add it to the settings file:") {
		t.Errorf("the hold inside Run never reached stdout:\n%s", out)
	}
}
