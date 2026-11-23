package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/model"
)

// lessonRunner maps each session to the lesson its prompt asked for and
// proposes a rule per open candidate, citing the candidate id the input
// shows, so two same-day runs propose different rules.
type lessonRunner struct{}

func (lessonRunner) Run(ctx context.Context, modelName, input string) (string, model.Usage, error) {
	title, rule := "Run go vet before done", "Run go vet before saying a change is done."
	if strings.Contains(input, "force push") {
		title, rule = "Ask before a force push", "Ask the owner before any force push."
	}
	if strings.Contains(input, "# CANDIDATES") {
		i := strings.Index(input, "Candidate[")
		if i < 0 {
			return "## Proposals\n\n(none)\n", model.Usage{}, nil
		}
		id := input[i+len("Candidate["):]
		id = id[:strings.Index(id, "]")]
		return fmt.Sprintf(`## Proposals

### add: %s
- target: ~/.claude/CLAUDE.md
- rule: %s
- evidence: %s
- why: the owner asked for it
`, title, rule, id), model.Usage{}, nil
	}
	return fmt.Sprintf("Observation[correction]: %s\n", rule), model.Usage{}, nil
}

// promptTranscript writes one claude session whose owner prompt is prompt,
// last active at when, with the file's mtime to match.
func promptTranscript(t *testing.T, f *hookFixture, session, prompt string, when time.Time) {
	t.Helper()
	dir := filepath.Join(f.home, ".claude", "projects", "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := when.UTC().Format(time.RFC3339)
	lines := fmt.Sprintf(`{"type":"user","timestamp":%q,"cwd":%q,"entrypoint":"cli","message":{"role":"user","content":%q}}
{"type":"assistant","timestamp":%q,"message":{"model":"claude-sonnet-4-5","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}}
`, ts, filepath.Join(f.home, "w"), prompt, ts)
	path := filepath.Join(dir, session+".jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// A second reduce the same day must not overwrite a run an apply already
// finished: the owner's morning `recall learn run` applies the day's dated
// directory, and the evening hook's run lands in a -2 sibling, applies it
// on its own branch, and stamps the learn step. Before, the second reduce
// reused the dated directory, the apply saw its apply.json and answered
// "already applied", and the new proposals were silently dropped while the
// hook stamped the step done.
func TestHookWorkerSecondSameDayRunAppliesFreshDir(t *testing.T) {
	f := newHookFixture(t)
	runner := lessonRunner{}
	f.env.runner = runner
	ctx := context.Background()

	// The morning, by hand: map a session, then run the loop.
	f.transcript(t, "sess-vet", f.clk.T.Add(-8*time.Hour))
	if code := learnMap(ctx, f.env, runner, "30d", 0, false, false, 0, nil); code != 0 {
		t.Fatalf("learn map = %d", code)
	}
	code, done := learnRun(ctx, f.env, runner, "", false)
	if code != 0 || !done {
		t.Fatalf("learn run = %d, done %v", code, done)
	}
	day := filepath.Join(learnStateRoot(f.env), f.clk.T.Format("2006-01-02"))
	if !exists(filepath.Join(day, "apply.json")) {
		t.Fatalf("the first run left no apply.json in %s", day)
	}
	if got := f.pending(t); got != 1 {
		t.Fatalf("pending reviews after the first run = %d, want 1", got)
	}

	// The evening, from the hook: a new session with a different lesson.
	f.clk.T = f.clk.T.Add(4 * time.Hour)
	promptTranscript(t, f, "sess-push", "ask me before any force push", f.clk.T.Add(-7*time.Hour))
	hookWork(f.env)

	rerun := day + "-2"
	md, err := os.ReadFile(filepath.Join(rerun, "proposals.md"))
	if err != nil {
		t.Fatalf("the second same-day reduce left no proposals in %s: %v", rerun, err)
	}
	if !strings.Contains(string(md), "force push") {
		t.Errorf("the -2 run's proposals don't carry the new lesson:\n%s", md)
	}
	if !exists(filepath.Join(rerun, "apply.json")) {
		t.Error("the -2 run never applied")
	}
	first, err := os.ReadFile(filepath.Join(day, "proposals.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "go vet") || strings.Contains(string(first), "force push") {
		t.Errorf("the applied first run was overwritten:\n%s", first)
	}
	if got := f.pending(t); got != 2 {
		t.Errorf("pending reviews after the second run = %d, want 2", got)
	}
	if !exists(f.stamp("learn")) {
		t.Error("the applied -2 run didn't stamp the learn step")
	}
}

// With the day's first run applied and its -2 sibling reduced but not yet
// applied, the default apply picks the -2 run, not the day's first.
func TestLatestRunPrefersNewestUnapplied(t *testing.T) {
	f := newHookFixture(t)
	root := learnStateRoot(f.env)
	day := filepath.Join(root, "2026-11-19")
	rerun := day + "-2"
	for _, dir := range []string{day, rerun} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "proposals.md"), []byte("## Proposals\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(day, "apply.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := latestRun(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != rerun {
		t.Errorf("latestRun = %s, want %s", got, rerun)
	}
	// With every run applied, the newest one answers, so an apply by hand
	// still says "already applied" instead of failing.
	if err := os.WriteFile(filepath.Join(rerun, "apply.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err = latestRun(root); err != nil || got != rerun {
		t.Errorf("latestRun with every run applied = %s, %v; want %s", got, err, rerun)
	}
}

// An unapplied run from an older day is never what a bare apply picks: a
// weeks-old reduce the owner chose not to apply doesn't beat the newest
// day's run, so the apply says "already applied" instead of staging stale
// proposals. --from reaches the old run explicitly.
func TestBareApplySkipsStaleOlderUnappliedRun(t *testing.T) {
	f := newHookFixture(t)
	root := learnStateRoot(f.env)
	stale := filepath.Join(root, "2026-11-01")
	newest := filepath.Join(root, "2026-11-18")
	for _, dir := range []string{stale, newest} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "proposals.md"), []byte("## Proposals\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(newest, "apply.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := latestRun(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != newest {
		t.Fatalf("latestRun = %s, want %s", got, newest)
	}
	code, done := applyRun(context.Background(), f.env, got, false)
	if code != 0 || done {
		t.Errorf("applyRun on the newest applied run = %d, done %v; want 0, false (already applied)", code, done)
	}
}

// A second reduce the same day never overwrites a run the owner hasn't
// applied yet: a hand-run morning reduce keeps its proposals, the evening
// reduce lands in the -2 sibling, and a following apply picks the -2 run
// as the day's newest unapplied.
func TestSecondSameDayReduceNeverOverwritesAnUnappliedRun(t *testing.T) {
	f := newHookFixture(t)
	runner := lessonRunner{}
	f.env.runner = runner
	ctx := context.Background()

	// The morning, by hand: map a session and reduce, but don't apply.
	f.transcript(t, "sess-vet", f.clk.T.Add(-8*time.Hour))
	if code := learnMap(ctx, f.env, runner, "30d", 0, false, false, 0, nil); code != 0 {
		t.Fatalf("learn map = %d", code)
	}
	if code := learnReduce(ctx, f.env, runner, "", false, false, false); code != 0 {
		t.Fatalf("learn reduce = %d", code)
	}
	day := filepath.Join(learnStateRoot(f.env), f.clk.T.Format("2006-01-02"))

	// The evening: a new session with a different lesson, another reduce,
	// still before the morning run is applied.
	f.clk.T = f.clk.T.Add(4 * time.Hour)
	promptTranscript(t, f, "sess-push", "ask me before any force push", f.clk.T.Add(-7*time.Hour))
	if code := learnMap(ctx, f.env, runner, "30d", 0, false, false, 0, nil); code != 0 {
		t.Fatalf("second learn map = %d", code)
	}
	if code := learnReduce(ctx, f.env, runner, "", false, false, false); code != 0 {
		t.Fatalf("second learn reduce = %d", code)
	}

	rerun := day + "-2"
	md, err := os.ReadFile(filepath.Join(rerun, "proposals.md"))
	if err != nil {
		t.Fatalf("the second same-day reduce didn't land in %s: %v", rerun, err)
	}
	if !strings.Contains(string(md), "force push") {
		t.Errorf("the -2 run's proposals don't carry the new lesson:\n%s", md)
	}
	first, err := os.ReadFile(filepath.Join(day, "proposals.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "go vet") || strings.Contains(string(first), "force push") {
		t.Errorf("the unapplied morning run was overwritten:\n%s", first)
	}
	got, err := latestRun(learnStateRoot(f.env))
	if err != nil {
		t.Fatal(err)
	}
	if got != rerun {
		t.Errorf("latestRun after the second reduce = %s, want %s", got, rerun)
	}
}

// An already-applied run isn't a success the caller may stamp: applyRun
// answers done=false, so the hook's learn step never counts a dropped run
// as the period's pass.
func TestApplyRunAlreadyAppliedIsNotDone(t *testing.T) {
	f := newHookFixture(t)
	run := filepath.Join(learnStateRoot(f.env), "2026-11-18")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"proposals.md": "## Proposals\n", "apply.json": "{}\n"} {
		if err := os.WriteFile(filepath.Join(run, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, done := applyRun(context.Background(), f.env, run, false)
	if code != 0 {
		t.Fatalf("applyRun on an applied run = %d, want 0", code)
	}
	if done {
		t.Error("an already-applied run counted as done")
	}
}

// A runner with no editor, like kiro, still reduces, but leaves the
// proposals for the owner instead of failing the apply, and the period
// counts as done so the scheduled loop doesn't pay for the same reduce
// again tomorrow.
func TestLearnRunWithoutAnEditorLeavesProposals(t *testing.T) {
	f := newHookFixture(t)
	runner := lessonRunner{}
	f.env.runner = runner
	f.env.editor = nil
	f.env.settings.Learn.Runner = "kiro"
	ctx := context.Background()

	f.transcript(t, "sess-vet", f.clk.T.Add(-8*time.Hour))
	if code := learnMap(ctx, f.env, runner, "30d", 0, false, false, 0, nil); code != 0 {
		t.Fatalf("learn map = %d", code)
	}
	code, done := learnRun(ctx, f.env, runner, "", false)
	if code != 0 || !done {
		t.Fatalf("learn run = %d, done %v; want 0, done", code, done)
	}
	day := filepath.Join(learnStateRoot(f.env), f.clk.T.Format("2006-01-02"))
	md, err := os.ReadFile(filepath.Join(day, "proposals.md"))
	if err != nil || !strings.Contains(string(md), "go vet") {
		t.Fatalf("proposals.md = %q, %v; want the reduce's proposal", md, err)
	}
	if exists(filepath.Join(day, "apply.json")) {
		t.Error("a run with no editor applied anyway")
	}
	if got := f.pending(t); got != 0 {
		t.Errorf("pending reviews = %d, want 0: nothing was staged", got)
	}
}
