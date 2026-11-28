package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/model"
)

// initRunner stands in for the harness CLI during init: the map observes
// the go vet correction with the owner's words and judges the rules it is
// shown (the force push rule broken where the owner pushed, the commit
// subject rule never relevant, the rest followed), and the reduce proposes a rule citing the first
// candidate it reads.
type initRunner struct {
	mu    sync.Mutex
	maps  int
	other int
}

func (r *initRunner) Run(ctx context.Context, _ model.Call, input string) (string, model.Usage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.Contains(input, "# CANDIDATES") {
		r.other++
		i := strings.Index(input, "Candidate[")
		if i < 0 {
			return "## Proposals\n\n(none)\n", model.Usage{}, nil
		}
		id := input[i+len("Candidate["):]
		id = id[:strings.Index(id, "]")]
		return fmt.Sprintf(`## Proposals

### add: Run go vet before done
- target: ~/.claude/CLAUDE.md
- rule: Run go vet before saying a change is done.
- evidence: %s
- why: the owner asked for it in several sessions
`, id), model.Usage{}, nil
	}
	if !strings.Contains(input, "# SESSION") {
		r.other++
		return "NONE\n", model.Usage{}, nil
	}
	r.maps++
	var out []string
	if !strings.Contains(input, "unrelated") {
		out = append(out, `Quote: "run go vet before you say it's done"`,
			"Observation[correction]: Run go vet before saying a change is done.")
	}
	if _, rules, ok := strings.Cut(input, "# CURRENT RULES"); ok {
		for _, line := range strings.Split(rules, "\n") {
			id, text, ok := strings.Cut(strings.TrimPrefix(line, "Rule["), "]")
			if !ok || !strings.HasPrefix(line, "Rule[") {
				continue
			}
			switch {
			case strings.Contains(text, "commit subjects"):
				// never comes up
			case strings.Contains(text, "force push") && strings.Contains(input, "then push"):
				out = append(out, "Rule["+id+"]: violated - pushed without asking")
			case strings.Contains(text, "force push"):
			default:
				out = append(out, "Rule["+id+"]: followed")
			}
		}
	}
	if len(out) == 0 {
		return "NONE\n", model.Usage{}, nil
	}
	return strings.Join(out, "\n") + "\n", model.Usage{}, nil
}

// initHome is a fresh machine: a temp home with a ~/.claude checkout whose
// CLAUDE.md holds rules, and claude sessions from the last few days.
type initHome struct {
	home   string
	runner *initRunner
}

func newInitHome(t *testing.T, sessions map[string]string) *initHome {
	t.Helper()
	isolateGit(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("RECALL_CONFIG", "")
	t.Setenv("RECALL_HOOK", "")
	dot := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(dot, "projects", "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	rules := "# Rules\n\n- Ask before any force push.\n- Keep commit subjects under 72 characters.\n- Run the tests before committing.\n"
	if err := os.WriteFile(filepath.Join(dot, "CLAUDE.md"), []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dot, ".gitignore"), []byte("projects/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dot, "init", "-q")
	gitIn(t, dot, "add", "-A")
	gitIn(t, dot, "commit", "-q", "-m", "seed")
	now := time.Now()
	day := 0
	for id, prompt := range sessions {
		day++
		when := now.Add(-time.Duration(day) * 24 * time.Hour)
		ts := when.UTC().Format(time.RFC3339)
		lines := fmt.Sprintf(`{"type":"user","timestamp":%q,"cwd":%q,"entrypoint":"cli","message":{"role":"user","content":%q}}
{"type":"assistant","timestamp":%q,"message":{"model":"claude-sonnet-4-5","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}}
`, ts, filepath.Join(home, "w"), prompt, ts)
		path := filepath.Join(dot, "projects", "p", id+".jsonl")
		if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	h := &initHome{home: home, runner: &initRunner{}}
	editor := &model.FakeEditor{Apply: func(input string, dirs []string) (string, error) {
		var out []string
		for _, line := range strings.Split(input, "\n") {
			if p, ok := strings.CutPrefix(line, "- edit: "); ok {
				data, err := os.ReadFile(p)
				if err != nil {
					return "", err
				}
				data = append(data, "- Run go vet before saying a change is done.\n"...)
				if err := os.WriteFile(p, data, 0o644); err != nil {
					return "", err
				}
				out = append(out, "Applied[add]: Run go vet before done -- "+p)
			}
		}
		return strings.Join(out, "\n"), nil
	}}
	oldEnv, oldIn := initEnv, initStdin
	t.Cleanup(func() { initEnv, initStdin = oldEnv, oldIn })
	initEnv = func() (*archiveEnv, error) {
		env, err := loadArchiveEnv()
		if err != nil {
			return nil, err
		}
		for i := range env.harnesses {
			env.harnesses[i] = stubInventoryHarness{env.harnesses[i]}
		}
		env.runner, env.editor = h.runner, editor
		return env, nil
	}
	return h
}

// runInit runs recall init with answer on its input and returns what it
// printed.
func (h *initHome) runInit(t *testing.T, answer string, args ...string) string {
	t.Helper()
	initStdin = strings.NewReader(answer)
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = out
	code := cmdInit(args)
	os.Stdout = stdout
	data, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("recall init exited %d:\n%s", code, data)
	}
	return string(data)
}

// vetSessions has five sessions with the same correction and one without,
// so halfway through any map order at least two of them are observed.
var vetSessions = map[string]string{
	"sess-a": "run go vet before you say it's done",
	"sess-b": "again: run go vet before you say it's done",
	"sess-c": "please run go vet before you say it's done, every time",
	"sess-e": "you forgot again; run go vet before you say it's done",
	"sess-f": "run go vet before you say it's done, then push",
	"sess-d": "an unrelated question about the weather",
}

// A fresh init offers the backfill with its cost, and on a yes maps the
// last 30 days, then on the reducer runs the reduce and apply, with no
// other command typed.
func TestInitBackfillMapsAndReduces(t *testing.T) {
	h := newInitHome(t, vetSessions)
	out := h.runInit(t, "y\n")
	t.Log(out)
	for _, want := range []string{
		"backfill: 6 session(s) from the last 30 days are unmapped; mapping them is 6 map call(s), about $0.14 (about 23k input tokens on sonnet at $2 in and $10 out per million, prices as of 2026-11-27)",
		"this machine is the store's reducer",
		"Map them now? [Y/n]",
		"/6] observed claude/",
		"observe: 6 observed",
		"proposals: ",
		"apply: 1 change(s) for review",
		"backfill: done in",
		"2. The last 30 days are mapped",
		"backfill: halfway there.",
		"What recall noticed in 6 session(s) since ",
		"  correction, in 5 sessions, ",
		"    Run go vet before saying a change is done.",
		`    you said: "run go vet before you say it's done"`,
		`proposed 20`,
		`as "Run go vet before done", waiting for review`,
		"recall report shows this again any time.",
		"Your rules, by what the same sessions show (3):",
		"  broken (1):\n    r-0001 ~/.claude/CLAUDE.md: Ask before any force push.\n      broken in 1, followed in 0, of 1 relevant session(s)\n",
		"  followed (1):\n    r-0003 ~/.claude/CLAUDE.md: Run the tests before committing.\n      followed in 6 of 6 relevant session(s)\n",
		"  never relevant (1):\n    r-0002 ~/.claude/CLAUDE.md: Keep commit subjects under 72 characters.\n      relevant in none of 6 judged session(s)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init output misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "recall observe --since 30d --dry-run") {
		t.Errorf("init still printed the map commands after the backfill:\n%s", out)
	}
	if h.runner.maps != 6 {
		t.Errorf("map calls = %d, want 6", h.runner.maps)
	}
	// The reduce ran here, so the hook's next reduce is a period away.
	if _, err := os.Stat(filepath.Join(h.home, ".local", "state", "recall", "hook-learn.stamp")); err != nil {
		t.Errorf("learn stamp: %v", err)
	}

	// Running init again finds nothing left to map and asks nothing.
	again := h.runInit(t, "")
	if !strings.Contains(again, "backfill: no session from the last 30 days needs mapping") || strings.Contains(again, "Map them now?") {
		t.Errorf("second init:\n%s", again)
	}
}

// Declining, or having no input to read, leaves init as it was: nothing
// mapped, and the map commands printed for later.
func TestInitBackfillDeclined(t *testing.T) {
	for name, answer := range map[string]string{"no": "n\n", "no input": ""} {
		t.Run(name, func(t *testing.T) {
			h := newInitHome(t, vetSessions)
			out := h.runInit(t, answer)
			if !strings.Contains(out, "Map them now?") || !strings.Contains(out, "backfill: skipped") {
				t.Errorf("no offer or no skip:\n%s", out)
			}
			if h.runner.maps+h.runner.other != 0 {
				t.Errorf("a declined backfill called the model %d time(s)", h.runner.maps+h.runner.other)
			}
			if !strings.Contains(out, "recall observe --since 30d && recall propose") {
				t.Errorf("declined init dropped the map commands:\n%s", out)
			}
		})
	}
}

// --backfill no skips the offer outright, for a scripted install, and
// --backfill yes maps without asking.
func TestInitBackfillFlag(t *testing.T) {
	h := newInitHome(t, vetSessions)
	out := h.runInit(t, "y\n", "--backfill", "no")
	if strings.Contains(out, "backfill") || h.runner.maps != 0 {
		t.Errorf("--backfill no offered or mapped (%d calls):\n%s", h.runner.maps, out)
	}

	h = newInitHome(t, vetSessions)
	out = h.runInit(t, "", "--backfill", "yes")
	if strings.Contains(out, "Map them now?") || h.runner.maps != 6 {
		t.Errorf("--backfill yes asked or didn't map (%d calls):\n%s", h.runner.maps, out)
	}

	if code := cmdInit([]string{"--backfill", "maybe"}); code != 2 {
		t.Errorf("--backfill maybe exited %d, want 2", code)
	}
}

// A machine whose store another host reduces for maps its own sessions
// and leaves the reduce to that host.
func TestInitBackfillMapsOnlyOffTheReducer(t *testing.T) {
	h := newInitHome(t, vetSessions)
	store := filepath.Join(h.home, "recall-store")
	if err := os.MkdirAll(filepath.Join(store, "learn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "learn", "reducer"), []byte("laptop-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.runInit(t, "y\n")
	if !strings.Contains(out, "another machine reduces for this store, so this one maps only") {
		t.Errorf("no maps-only note:\n%s", out)
	}
	if h.runner.maps != 6 || h.runner.other != 0 {
		t.Errorf("map calls %d, other calls %d; want 6 and 0", h.runner.maps, h.runner.other)
	}
	if strings.Contains(out, "proposals: ") {
		t.Errorf("a non-reducer reduced:\n%s", out)
	}
}

func TestMapEstimate(t *testing.T) {
	s := kiroSettings(true)
	s.Learn.Runner, s.Learn.MapModel, s.Learn.MapWorkers = "kiro", "auto", 4
	if got, want := mapEstimate(s, 100, 1_000_000, 0), "100 map call(s), about 10.0 credits (0.1 credits each on auto, prices as of 2026-11-27) and 4 minute(s) at 4 at a time"; got != want {
		t.Errorf("kiro estimate = %q, want %q", got, want)
	}
	// 2M chars is 500k input tokens; each of 10 calls adds 4k chars of
	// context (1k tokens) and the CLI's prompt; all at $2, plus 10 calls'
	// output allowance at $10.
	s.Learn.Runner, s.Learn.MapModel = "claude", "sonnet"
	in := 500_000 + 10*(1000+config.CLIPromptTokens("claude"))
	cost := float64(in)*2/1e6 + 10*config.ObserveOutputTokens*10/1e6
	want := fmt.Sprintf("10 map call(s), about $%.2f (about %s input tokens on sonnet at $2 in and $10 out per million, prices as of 2026-11-27) and 1 minute(s) at 4 at a time", cost, roundTokens(in))
	if got := mapEstimate(s, 10, 2_000_000, 4000); got != want {
		t.Errorf("claude estimate = %q, want %q", got, want)
	}
	s.Learn.MapModel = "sonnet-9"
	if got, want := mapEstimate(s, 1, 100, 0), "1 map call(s), no price for model sonnet-9 in prices.json; about 1 minute(s) at 4 at a time"; got != want {
		t.Errorf("unpriced model estimate = %q, want %q", got, want)
	}
	s.Learn.Runner = "unknown"
	if got, want := mapEstimate(s, 1, 100, 0), "1 map call(s), no price for model sonnet-9 in prices.json; about 1 minute(s) at 4 at a time"; got != want {
		t.Errorf("unknown runner estimate = %q, want %q", got, want)
	}
}

// Init's plan names the runner the loop will call, set or not.
func TestInitNamesRunner(t *testing.T) {
	h := newInitHome(t, nil)
	out := h.runInit(t, "", "--dry-run")
	if !strings.Contains(out, "runner: claude, learn.runner's default for the enabled harnesses; observe, propose, and apply call claude\n") {
		t.Errorf("no default runner line:\n%s", out)
	}
	if strings.Contains(out, "set learn.runner") {
		t.Errorf("init plans learn.runner:\n%s", out)
	}
	dir := filepath.Join(h.home, ".config", "recall")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[harness.kiro]\n[learn]\nrunner = \"kiro\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := h.runInit(t, "", "--dry-run"); !strings.Contains(out, "runner: kiro, as configured; observe, propose, and apply call kiro-cli\n") {
		t.Errorf("no configured runner line:\n%s", out)
	}
}

// The context the runner's CLI loads into every call counts in characters.
func TestLoadedContextChars(t *testing.T) {
	home := t.TempDir()
	if n := loadedContextChars("claude", home); n != 0 {
		t.Errorf("no context file = %d chars", n)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "CLAUDE.md"), []byte("- Vérifie ça.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := loadedContextChars("claude", home); n != 14 {
		t.Errorf("context = %d chars, want 14", n)
	}
	if n := loadedContextChars("unknown", home); n != 0 {
		t.Errorf("an unknown runner's context = %d chars", n)
	}
}
