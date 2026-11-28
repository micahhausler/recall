package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/evidence"
	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/harness/adapters"
	"github.com/micahhausler/recall/internal/hook"
	"github.com/micahhausler/recall/internal/learn"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/review"
)

// isolateGit keeps git away from the owner's global config and gives
// commits an identity.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "you")
	t.Setenv("GIT_COMMITTER_NAME", "you")
	t.Setenv("GIT_AUTHOR_EMAIL", "you@example.com")
	t.Setenv("GIT_COMMITTER_EMAIL", "you@example.com")
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// stubInventoryHarness is the real claude harness with the one method that
// starts a harness CLI stubbed out; no test runs a model.
type stubInventoryHarness struct{ harness.Harness }

func (stubInventoryHarness) Inventory(ctx context.Context, dir string) (harness.Inventory, error) {
	return harness.Inventory{}, nil
}

// scriptedRunner answers the map and the reduce apart: a reduce input (it
// carries the CANDIDATES section) gets a proposal only when it has a
// candidate to cite, the way a real reduce has nothing to say over nothing.
type scriptedRunner struct {
	mu    sync.Mutex
	calls []string
}

const scriptedProposal = `## Proposals

### add: Run go vet before done
- target: ~/.claude/CLAUDE.md
- rule: Run go vet before saying a change is done.
- evidence: c-0001
- why: the owner asked for it
`

func (r *scriptedRunner) Run(ctx context.Context, _ model.Call, input string) (string, model.Usage, error) {
	r.mu.Lock()
	r.calls = append(r.calls, input)
	r.mu.Unlock()
	if strings.Contains(input, "# CANDIDATES") {
		if strings.Contains(input, "Candidate[") {
			return scriptedProposal, model.Usage{}, nil
		}
		return "## Proposals\n\n(none)\n", model.Usage{}, nil
	}
	return "Observation[correction]: Run go vet before saying a change is done.\n", model.Usage{}, nil
}

// hookFixture is a hook worker against a temp home: fake clock, scripted
// runner, fake editor, a real fs store under home, review on a local
// branch of the ~/.claude checkout, and the real claude harness reading
// transcripts the test writes.
type hookFixture struct {
	home   string
	env    *archiveEnv
	clk    *clock.Fake
	runner *scriptedRunner
	editor *model.FakeEditor
}

func newHookFixture(t *testing.T) *hookFixture {
	t.Helper()
	return newHookFixtureWith(t, "[learn]\nreview = \"branch\"\n\n[hooks]\nmap = true\nlearn_every = 7\n").noOwnerEvidence()
}

// noOwnerEvidence turns the owner-evidence check off.
func (f *hookFixture) noOwnerEvidence() *hookFixture {
	f.env.noOwnerEvidence = true
	return f
}

func newHookFixtureWith(t *testing.T, toml string) *hookFixture {
	t.Helper()
	isolateGit(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".config", "recall", "config.toml")
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(base)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := c.Settings(home, "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	hs, err := adapters.Enabled(settings)
	if err != nil {
		t.Fatal(err)
	}
	for i := range hs {
		hs[i] = stubInventoryHarness{hs[i]}
	}
	f := &hookFixture{
		home:   home,
		clk:    &clock.Fake{T: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)},
		runner: &scriptedRunner{},
	}
	f.editor = &model.FakeEditor{Apply: func(input string, dirs []string) (string, error) {
		var out []string
		for _, line := range strings.Split(input, "\n") {
			if p, ok := strings.CutPrefix(line, "- edit: "); ok {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return "", err
				}
				if err := os.WriteFile(p, []byte("# Rules\n\n- Run go vet before saying a change is done.\n"), 0o644); err != nil {
					return "", err
				}
				out = append(out, "Applied[add]: Run go vet before done -- "+p)
			}
		}
		return strings.Join(out, "\n"), nil
	}}
	f.env = &archiveEnv{home: home, settings: settings, harnesses: hs, clk: f.clk, runner: f.runner, editor: f.editor}
	// The target has to sit in a git checkout: review stages its edits on a
	// branch of it.
	dot := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dot, "CLAUDE.md"), []byte("# Rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dot, "init", "-q")
	gitIn(t, dot, "add", "-A")
	gitIn(t, dot, "commit", "-q", "-m", "seed")
	return f
}

// transcript writes one claude session the owner typed into, last active at
// when, and sets the file's mtime to match so the idle check sees it.
func (f *hookFixture) transcript(t *testing.T, session string, when time.Time) {
	t.Helper()
	dir := filepath.Join(f.home, ".claude", "projects", "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := when.UTC().Format(time.RFC3339)
	lines := fmt.Sprintf(`{"type":"user","timestamp":%q,"cwd":%q,"entrypoint":"cli","message":{"role":"user","content":"always run go vet before saying a change is done"}}
{"type":"assistant","timestamp":%q,"message":{"model":"claude-sonnet-4-5","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}}
`, ts, filepath.Join(f.home, "w"), ts)
	path := filepath.Join(dir, session+".jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// pending counts the proposals waiting for review on the ledger, which the
// apply records when it leaves a branch.
func (f *hookFixture) pending(t *testing.T) int {
	t.Helper()
	l, err := learn.LoadLedger(config.LedgerPath(f.env.home))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, rv := range l.Reviews {
		if rv.Verdict == review.Pending {
			n++
		}
	}
	return n
}

func (f *hookFixture) stamp(name string) string {
	return filepath.Join(hookStateDir(f.env), "hook-"+name+".stamp")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A fresh install with transcripts already on disk gets its first proposal
// from the first worker run: the map reaches back over the retained
// transcripts, bounded by the window `recall init` prints, instead of
// starting from the moment the hook first fired.
func TestHookWorkerFirstRunMapsBackfill(t *testing.T) {
	f := newHookFixture(t)
	f.transcript(t, "sess-old", f.clk.T.AddDate(0, 0, -5))

	hookWork(f.env)

	mapped := false
	for _, in := range f.runner.calls {
		if strings.Contains(in, "always run go vet") && !strings.Contains(in, "# CANDIDATES") {
			mapped = true
		}
	}
	if !mapped {
		t.Error("the first run never mapped the retained transcript")
	}
	if got := f.pending(t); got != 1 {
		t.Errorf("pending reviews after the first run = %d, want 1", got)
	}
	for _, p := range []string{"ledger.json", "learn/candidates.json"} {
		if !exists(filepath.Join(f.home, ".local", "state", "recall", p)) {
			t.Errorf("%s isn't under the home", p)
		}
	}
	if !exists(f.stamp("learn")) {
		t.Error("a reduce that read candidates didn't stamp the learn step")
	}
	// The recorded start is bounded: the default window back, not all of
	// history and not the moment the hook fired.
	data, err := os.ReadFile(filepath.Join(hookStateDir(f.env), "hook-map.since"))
	if err != nil {
		t.Fatal(err)
	}
	since, err := time.Parse(time.RFC3339, string(data))
	if err != nil {
		t.Fatal(err)
	}
	if want := f.clk.T.AddDate(0, 0, -mapWindowDays); !since.Equal(want) {
		t.Errorf("recorded since = %v, want %v", since, want)
	}
}

// A reduce with no candidates to read doesn't stamp the learn step as done,
// so the first sessions to arrive are reduced the next day, not a week out.
func TestHookWorkerLearnWaitsForCandidates(t *testing.T) {
	f := newHookFixture(t)

	// Day 0: nothing to map, so the reduce reads nothing.
	hookWork(f.env)
	if exists(f.stamp("learn")) {
		t.Error("a reduce over nothing was stamped as the week's run")
	}
	if !exists(f.stamp("learn-tried")) {
		t.Error("the empty reduce wasn't marked as tried; it would re-run each session end")
	}
	if got := f.pending(t); got != 0 {
		t.Errorf("pending reviews with nothing mapped = %d, want 0", got)
	}

	// A day later a session exists; the next worker run proposes from it
	// without waiting out hooks.learn_every.
	f.clk.T = f.clk.T.Add(25 * time.Hour)
	f.transcript(t, "sess-new", f.clk.T.Add(-7*time.Hour))
	hookWork(f.env)
	if got := f.pending(t); got != 1 {
		t.Errorf("pending reviews a day after install = %d, want 1", got)
	}
	if !exists(f.stamp("learn")) {
		t.Error("the reduce that read candidates didn't stamp the learn step")
	}
}

// With learn.evidence set, the worker scans each source at most once a day,
// like the archive step: a second session ending the same day doesn't re-run
// the source's subprocess calls. `recall observe` by hand always scans.
func TestHookWorkerEvidenceScansOncePerDay(t *testing.T) {
	src := &evidence.Fake{}
	evidence.Register("fake", func(o evidence.Options) (evidence.Source, error) { return src, nil })
	f := newHookFixtureWith(t, "[learn]\nreview = \"branch\"\nevidence = [\"fake\"]\n\n[hooks]\nmap = true\nlearn_every = 7\n").noOwnerEvidence()

	hookWork(f.env)
	if got := len(src.Since); got != 1 {
		t.Fatalf("evidence scans after the first worker run = %d, want 1", got)
	}
	f.clk.T = f.clk.T.Add(2 * time.Hour)
	hookWork(f.env)
	if got := len(src.Since); got != 1 {
		t.Errorf("evidence scans after a second worker run the same day = %d, want 1", got)
	}
	f.clk.T = f.clk.T.Add(23 * time.Hour)
	hookWork(f.env)
	if got := len(src.Since); got != 2 {
		t.Errorf("evidence scans the next day = %d, want 2", got)
	}
}

// A machine with a recall recur job for a step never runs that step from a
// hook as well. The job's command decides, step by step, so an unrelated
// job blocks nothing.
func TestHookWorkerSkipsStepsRecurJobsRun(t *testing.T) {
	job := func(command string) string {
		return "[recur.jobs.weekly]\nschedule = \"0 12 * * 1\"\ncommand = \"" + command + "\"\n"
	}
	base := "[learn]\nreview = \"branch\"\n\n[hooks]\nmap = true\narchive = true\nlearn_every = 7\n\n"
	fixture := func(t *testing.T, toml string) *hookFixture {
		return newHookFixtureWith(t, toml).noOwnerEvidence()
	}

	t.Run("map", func(t *testing.T) {
		f := fixture(t, base+job("recall learn map --since 15d"))
		f.transcript(t, "sess", f.clk.T.AddDate(0, 0, -2))
		hookWork(f.env)
		for _, in := range f.runner.calls {
			if !strings.Contains(in, "# CANDIDATES") {
				t.Error("the map ran from the hook beside the recur job that runs it")
			}
		}
		if exists(filepath.Join(hookStateDir(f.env), "hook-map.since")) {
			t.Error("the skipped map step still recorded a start")
		}
	})
	t.Run("observe", func(t *testing.T) {
		f := fixture(t, base+job("recall observe --idle 6h"))
		f.transcript(t, "sess", f.clk.T.AddDate(0, 0, -2))
		hookWork(f.env)
		for _, in := range f.runner.calls {
			if !strings.Contains(in, "# CANDIDATES") {
				t.Error("the map ran from the hook beside the recur job that observes")
			}
		}
		if exists(filepath.Join(hookStateDir(f.env), "hook-map.since")) {
			t.Error("the skipped map step still recorded a start")
		}
	})
	t.Run("archive", func(t *testing.T) {
		f := fixture(t, base+job("recall archive-chats run"))
		hookWork(f.env)
		if exists(f.stamp("archive")) {
			t.Error("the archive ran from the hook beside the recur job that runs it")
		}
	})
	t.Run("archive with a redirect", func(t *testing.T) {
		f := fixture(t, base+job("recall archive >> /tmp/recall.log 2>&1"))
		hookWork(f.env)
		if exists(f.stamp("archive")) {
			t.Error("the archive ran from the hook beside the recur job that runs it with a redirect")
		}
	})
	t.Run("learn", func(t *testing.T) {
		f := fixture(t, base+job("recall learn run"))
		f.transcript(t, "sess", f.clk.T.AddDate(0, 0, -2))
		hookWork(f.env)
		for _, in := range f.runner.calls {
			if strings.Contains(in, "# CANDIDATES") {
				t.Error("the reduce ran from the hook beside the recur job that runs it")
			}
		}
		if exists(f.stamp("learn")) || exists(f.stamp("learn-tried")) {
			t.Error("the skipped learn step was stamped")
		}
		if got := f.pending(t); got != 0 {
			t.Errorf("pending reviews = %d, want none from the hook", got)
		}
	})
	t.Run("unrelated job blocks nothing", func(t *testing.T) {
		f := fixture(t, base+job("backup-home"))
		hookWork(f.env)
		if !exists(f.stamp("archive")) {
			t.Error("an unrelated recur job kept the archive hook step from running")
		}
	})
	t.Run("archive plumbing blocks nothing", func(t *testing.T) {
		f := fixture(t, base+job("recall archive-chats check; recall archive build 2026-W45 out.tar.zst"))
		hookWork(f.env)
		if !exists(f.stamp("archive")) {
			t.Error("a job that only checks or builds kept the archive hook step from running")
		}
	})
}

func TestHookSessionStart(t *testing.T) {
	home := review.Resolve(t.TempDir())
	upstream := filepath.Join(home, "src", "upstream")
	if err := os.MkdirAll(filepath.Join(upstream, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upstream, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@example.com:them/upstream.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "src", "notes", "repo-notes")
	note := filepath.Join(dir, "example.com", "them", "upstream.md")
	if err := os.MkdirAll(filepath.Dir(note), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(note, []byte("- Run make test.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &archiveEnv{home: home, settings: &config.Settings{Learn: config.LearnSettings{RepoNotes: dir}}}

	var out strings.Builder
	if err := sessionStart(hook.Input{CWD: upstream, Event: "SessionStart"}, env, &out); err != nil {
		t.Fatal(err)
	}
	var got struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if got.HookSpecificOutput.HookEventName != "SessionStart" || !strings.Contains(got.HookSpecificOutput.AdditionalContext, "- Run make test.") {
		t.Errorf("answer = %+v", got)
	}

	for name, e := range map[string]struct {
		cwd string
		env *archiveEnv
	}{
		"another directory": {home, env},
		"repo notes off":    {upstream, &archiveEnv{home: home, settings: &config.Settings{}}},
	} {
		out.Reset()
		if err := sessionStart(hook.Input{CWD: e.cwd}, e.env, &out); err != nil || out.Len() != 0 {
			t.Errorf("%s: printed %q, %v", name, out.String(), err)
		}
	}
}

// One of recall's own harness runs gets no repo note: the hook returns
// before it reads anything.
func TestHookSessionStartGuarded(t *testing.T) {
	home := t.TempDir()
	upstream := filepath.Join(home, "src", "upstream")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, upstream, "init", "-q")
	gitIn(t, upstream, "remote", "add", "origin", "git@example.com:them/upstream.git")
	dir := filepath.Join(home, "notes", "repo-notes")
	note := filepath.Join(dir, "example.com", "them", "upstream.md")
	if err := os.MkdirAll(filepath.Dir(note), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(note, []byte("- Run make test.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("host = \"laptop-a\"\n[learn]\nrepo_notes = %q\n", dir)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(config.EnvVar, cfg)
	input := fmt.Sprintf(`{"cwd":%q,"hook_event_name":"SessionStart"}`, upstream)
	hookRun := func() (int, string) {
		in, inW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		inW.WriteString(input)
		inW.Close()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stdin, stdout := os.Stdin, os.Stdout
		os.Stdin, os.Stdout = in, w
		code := run([]string{"hook", "session-start"})
		os.Stdin, os.Stdout = stdin, stdout
		w.Close()
		data, _ := io.ReadAll(r)
		return code, string(data)
	}
	// Unguarded, the same input prints the note, so the guard is what keeps
	// recall's own runs quiet.
	if code, out := hookRun(); code != 0 || !strings.Contains(out, "Run make test.") {
		t.Fatalf("unguarded session-start: code %d, printed %q", code, out)
	}
	t.Setenv(hook.GuardEnv, "1")
	if code, out := hookRun(); code != 0 || out != "" {
		t.Errorf("guarded session-start: code %d, printed %q", code, out)
	}
}

// Repo notes may live in any git checkout, listed in learn.repos or not,
// and the directory needn't exist yet.
func TestLoadReposRepoNotes(t *testing.T) {
	home := t.TempDir()
	notes := filepath.Join(home, "src", "notes")
	if err := os.MkdirAll(notes, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, notes, "init", "-q")
	loose := filepath.Join(home, "loose")
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dir, err string
	}{
		{filepath.Join(notes, "repo-notes"), ""},
		{filepath.Join(loose, "repo-notes"), "isn't in a git checkout"},
	} {
		env := &archiveEnv{home: home, settings: &config.Settings{Learn: config.LearnSettings{RepoNotes: c.dir}}}
		_, err := loadRepos(context.Background(), env)
		if c.err == "" && err != nil {
			t.Errorf("%s: %v", c.dir, err)
		}
		if c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: err = %v, want %q", c.dir, err, c.err)
		}
	}
}
