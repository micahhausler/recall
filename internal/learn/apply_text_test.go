package learn

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/model"
)

// runnerFunc is a Runner whose answer depends on its input.
type runnerFunc func(input string) (string, error)

func (f runnerFunc) Run(_ context.Context, _, input string) (string, model.Usage, error) {
	out, err := f(input)
	return out, model.Usage{}, err
}

// textBlock is one file in a text-mode answer.
func textBlock(path, content string) string {
	return defaultMarker + " BEGIN " + path + "\n" + content + defaultMarker + " END " + path + "\n"
}

const branchFirst = "## Proposals\n\n### add: Branch first\n- target: ~/dotfiles/.claude/CLAUDE.md\n" +
	"- rule: Branch before committing.\n- evidence: s1, s2\n"

// textFixture is an apply fixture with no editor: answer stands in for the
// runner, given the plan and the input.
func textFixture(t *testing.T, proposals string, answer func(plan *Plan, input string) string) (*applyFixture, *Plan, *[]string, *bytes.Buffer) {
	t.Helper()
	f := newApplyFixture(t, proposals)
	plan, err := f.applier.Plan(context.Background(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []string
	var log bytes.Buffer
	f.applier.Editor = nil
	f.applier.Runner = runnerFunc(func(input string) (string, error) {
		inputs = append(inputs, input)
		return answer(plan, input), nil
	})
	f.applier.Out = &log
	return f, plan, &inputs, &log
}

func dotTarget(plan *Plan) string {
	return filepath.Join(plan.Worktrees[0], ".claude", "CLAUDE.md")
}

func TestApplyTextModeWritesAndCommits(t *testing.T) {
	ctx := context.Background()
	f, plan, inputs, _ := textFixture(t, mixedProposals, func(plan *Plan, input string) string {
		kit := plan.Worktrees[1]
		return textBlock(dotTarget(plan), "# Rules\n\n- Branch before committing.\n- See check-time.\n") +
			textBlock(filepath.Join(kit, "skills", "practice", "SKILL.md"), "---\nname: practice\n---\nsteps\n") +
			textBlock(filepath.Join(kit, "bin", "check-time"), "#!/bin/sh\nexit 0\n") +
			"Applied[add]: Branch first -- " + dotTarget(plan) + "\n" +
			"Applied[skill]: Timed practice -- " + filepath.Join(kit, "skills", "practice", "SKILL.md") + "\n" +
			"Applied[graduate]: Check the time math -- " + filepath.Join(kit, "bin", "check-time") + ", " + dotTarget(plan) + "\n"
	})
	// The kit's own conventions, which the input shows read-only.
	writeFile(t, filepath.Join(f.kit, "AGENTS.md"), "Keep lines short.\n")
	gitIn(t, f.kit, "add", "AGENTS.md")
	gitIn(t, f.kit, "commit", "-q", "-m", "conventions")
	gitIn(t, f.kit, "push", "-q", "origin", "HEAD:main")

	res, err := f.applier.Run(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(*inputs) != 1 || len(f.editor.Calls) != 0 {
		t.Fatalf("runner calls %d, editor calls %d", len(*inputs), len(f.editor.Calls))
	}
	in := (*inputs)[0]
	kit := plan.Worktrees[1]
	for _, want := range []string{
		"You have no tools.",
		"\n# FILES\n",
		"- " + dotTarget(plan) + "\n",
		"- " + filepath.Join(kit, "bin", "check-time") + " (doesn't exist yet",
		defaultMarker + " BEGIN " + dotTarget(plan) + "\n# Rules\n\n- one\n" + defaultMarker + " END " + dotTarget(plan) + "\n",
		"Read only: follow these, never return them.\n- " + filepath.Join(kit, "AGENTS.md") + "\n",
		defaultMarker + " BEGIN " + filepath.Join(kit, "AGENTS.md") + "\nKeep lines short.\n",
	} {
		if !strings.Contains(in, want) {
			t.Errorf("input misses %q:\n%s", want, in)
		}
	}
	if strings.Contains(in, shadowDir+"/") {
		t.Error("text mode shadowed a .claude directory; the binary writes there itself")
	}

	if len(res.Changes) != 2 {
		t.Fatalf("changes = %+v", res.Changes)
	}
	if got := gitIn(t, f.dot, "show", plan.Branch+":.claude/CLAUDE.md"); !strings.Contains(got, "- Branch before committing.") {
		t.Errorf("branch CLAUDE.md = %q", got)
	}
	if live, _ := os.ReadFile(filepath.Join(f.dot, ".claude", "CLAUDE.md")); string(live) != "# Rules\n\n- one\n" {
		t.Errorf("live file changed: %q", live)
	}
	if got := gitIn(t, f.kit, "show", plan.Branch+":skills/practice/SKILL.md"); !strings.Contains(got, "name: practice") {
		t.Errorf("branch skill = %q", got)
	}
	if mode := gitIn(t, f.kit, "ls-tree", plan.Branch, "bin/check-time"); !strings.HasPrefix(mode, "100755") {
		t.Errorf("script not executable: %s", mode)
	}
}

// With an editor, apply uses it, even when a runner is set too.
func TestApplyUsesTheEditorWhenThereIsOne(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, branchFirst)
	ran := false
	f.applier.Runner = runnerFunc(func(string) (string, error) { ran = true; return "", nil })
	plan, err := f.applier.Plan(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.applier.Run(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if ran || len(f.editor.Calls) != 1 {
		t.Fatalf("runner ran %v, editor calls %d", ran, len(f.editor.Calls))
	}
	if in := f.editor.Calls[0].Input; strings.Contains(in, "# FILES") || strings.Contains(in, "You have no tools.") {
		t.Error("the editor got a text-mode input")
	}
}

// Every path the binary didn't list as editable is refused, and nothing is
// written outside the listed files; the proposal the answer applied
// cleanly still lands.
func TestApplyTextModeRefusesUnlistedPaths(t *testing.T) {
	for name, escape := range map[string]func(plan *Plan, base string) string{
		"unlisted in the worktree": func(plan *Plan, _ string) string {
			return filepath.Join(plan.Worktrees[0], ".claude", "other.md")
		},
		"absolute outside": func(_ *Plan, base string) string { return filepath.Join(base, "outside", "evil.md") },
		"dot-dot escape": func(plan *Plan, _ string) string {
			return plan.Worktrees[0] + "/../../../outside/evil.md"
		},
		"relative": func(*Plan, string) string { return ".claude/other.md" },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var bad string
			f, plan, _, log := textFixture(t, branchFirst, func(plan *Plan, input string) string {
				return textBlock(dotTarget(plan), "# Rules\n\n- one\n- Branch before committing.\n") +
					textBlock(bad, "pwned\n") +
					"Applied[add]: Branch first -- " + dotTarget(plan) + "\n"
			})
			bad = escape(plan, f.home)
			if err := os.MkdirAll(filepath.Join(f.home, "outside"), 0o755); err != nil {
				t.Fatal(err)
			}
			res, err := f.applier.Run(ctx, plan)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(log.String(), "refused to write "+bad) {
				t.Errorf("no refusal reported:\n%s", log.String())
			}
			for _, p := range []string{filepath.Join(f.home, "outside", "evil.md"), filepath.Join(plan.Worktrees[0], ".claude", "other.md"),
				filepath.Join(f.home, ".claude", "other.md")} {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("%s was written", p)
				}
			}
			if len(res.Changes) != 1 || len(res.Changes[0].Proposals) != 1 {
				t.Fatalf("the clean proposal didn't land: %+v", res.Changes)
			}
			if got := gitIn(t, f.dot, "show", "--stat", "--format=", plan.Branch); strings.Contains(got, "other.md") {
				t.Errorf("the branch carries the refused file: %s", got)
			}
		})
	}
}

// A proposal whose outcome names a refused path is not applied, and its
// other files are withheld; so is a file the answer returns twice.
func TestApplyTextModeSkipsAProposalNamingARefusedPath(t *testing.T) {
	for name, answer := range map[string]func(plan *Plan, base string) string{
		"names an outside path": func(plan *Plan, base string) string {
			evil := filepath.Join(base, "evil.md")
			return textBlock(dotTarget(plan), "# Rules\n\n- changed\n") + textBlock(evil, "pwned\n") +
				"Applied[add]: Branch first -- " + dotTarget(plan) + ", " + evil + "\n"
		},
		"returns a file twice": func(plan *Plan, _ string) string {
			return textBlock(dotTarget(plan), "# Rules\n\n- changed\n") + textBlock(dotTarget(plan), "# Rules\n\n- again\n") +
				"Applied[add]: Branch first -- " + dotTarget(plan) + "\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var base string
			f, plan, _, _ := textFixture(t, branchFirst, func(plan *Plan, _ string) string { return answer(plan, base) })
			base = f.home
			res, err := f.applier.Run(ctx, plan)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Changes) != 0 {
				t.Errorf("a change landed: %+v", res.Changes)
			}
			if len(res.Outcomes) != 1 || res.Outcomes[0].Applied || !strings.Contains(res.Outcomes[0].Detail, "refused to write") {
				t.Errorf("outcomes = %+v", res.Outcomes)
			}
			if _, err := os.Stat(filepath.Join(f.home, "evil.md")); err == nil {
				t.Error("the outside file was written")
			}
		})
	}
}

// The worktree comes from the remote, so it can hold a symlink the live
// checkout doesn't: a target that is itself a symlink, or that sits in a
// symlinked directory, pointing outside. Neither is shown or written.
func TestApplyTextModeRefusesSymlinksOutOfTheWorktree(t *testing.T) {
	for name, link := range map[string]func(t *testing.T, dot, outside string){
		"symlinked target": func(t *testing.T, dot, outside string) {
			if err := os.Remove(filepath.Join(dot, ".claude", "CLAUDE.md")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "CLAUDE.md"), filepath.Join(dot, ".claude", "CLAUDE.md")); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked directory": func(t *testing.T, dot, outside string) {
			if err := os.RemoveAll(filepath.Join(dot, ".claude")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dot, ".claude")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f, plan, inputs, log := textFixture(t, branchFirst, func(plan *Plan, input string) string {
				return textBlock(dotTarget(plan), "pwned\n") + "Applied[add]: Branch first -- " + dotTarget(plan) + "\n"
			})
			outside := filepath.Join(f.home, "outside")
			writeFile(t, filepath.Join(outside, "CLAUDE.md"), "outside-only-text\n")
			// Push the symlink, then put the live checkout back as it was.
			link(t, f.dot, outside)
			gitIn(t, f.dot, "add", "-A")
			gitIn(t, f.dot, "commit", "-q", "-m", "link")
			gitIn(t, f.dot, "push", "-q", "origin", "HEAD:main")
			gitIn(t, f.dot, "reset", "-q", "--hard", "HEAD~1")

			res, err := f.applier.Run(ctx, plan)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(filepath.Join(outside, "CLAUDE.md")); string(got) != "outside-only-text\n" {
				t.Errorf("the file outside the worktree = %q", got)
			}
			in := (*inputs)[0]
			if strings.Contains(in, "outside-only-text") {
				t.Error("the input shows a file outside the worktree")
			}
			if !strings.Contains(in, "- "+dotTarget(plan)+": it is a symlink") && !strings.Contains(in, "outside the worktree") {
				t.Errorf("the input doesn't say why the target isn't editable:\n%s", in)
			}
			if !strings.Contains(in, "Not editable: ") {
				t.Errorf("the input doesn't mark the target not editable:\n%s", in)
			}
			if !strings.Contains(log.String(), "refused to write") {
				t.Errorf("no refusal reported:\n%s", log.String())
			}
			if len(res.Changes) != 0 || len(res.Outcomes) != 1 || res.Outcomes[0].Applied {
				t.Errorf("changes %+v, outcomes %+v", res.Changes, res.Outcomes)
			}
		})
	}
}

// A malformed answer fails the apply the way a failed editor call does:
// nothing published, no branch kept, and no apply.json, so a retry runs.
func TestApplyTextModeMalformedAnswerFails(t *testing.T) {
	for name, answer := range map[string]func(plan *Plan) string{
		"unterminated block": func(plan *Plan) string {
			return defaultMarker + " BEGIN " + dotTarget(plan) + "\n# Rules\nApplied[add]: Branch first -- " + dotTarget(plan) + "\n"
		},
		"no outcome lines": func(plan *Plan) string { return textBlock(dotTarget(plan), "# Rules\n") },
		"end without begin": func(plan *Plan) string {
			return defaultMarker + " END " + dotTarget(plan) + "\nApplied[add]: Branch first -- " + dotTarget(plan) + "\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f, plan, _, _ := textFixture(t, branchFirst, func(plan *Plan, _ string) string { return answer(plan) })
			if _, err := f.applier.Run(ctx, plan); err == nil || !strings.Contains(err.Error(), "malformed answer") {
				t.Fatalf("err = %v", err)
			}
			if out := gitIn(t, f.dot, "branch", "--list", plan.Branch); out != "" {
				t.Errorf("branch kept after a malformed answer: %s", out)
			}
			if _, err := os.Stat(filepath.Join(f.run, "apply.json")); !os.IsNotExist(err) {
				t.Error("apply.json written for a failed run")
			}
			if len(f.pub.Changes) != 0 {
				t.Error("published after a malformed answer")
			}
		})
	}
}

// The marker never collides with a line the input already holds.
func TestTextMarkerAvoidsTheInput(t *testing.T) {
	tf := &textFiles{entries: []textEntry{{path: "/wt/a", content: []byte(defaultMarker + " BEGIN /wt/a\n")}}}
	tf.pickMarker("mentions %%recall-file-2%% too")
	if tf.marker != "%%recall-file-3%%" {
		t.Errorf("marker = %q", tf.marker)
	}
}

// An existing file keeps its mode when the binary rewrites it.
func TestWriteConfinedKeepsMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(p, []byte("old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeConfined(p, dir, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, %v", st.Mode(), err)
	}
	if err := writeConfined(filepath.Join(dir, "a", "b", "new.md"), dir, []byte("x\n")); err != nil {
		t.Errorf("creating under new dirs: %v", err)
	}
}

// A proposal whose files would push the input past the budget is held back
// at plan time.
func TestTextModeHoldsBackOversizedFiles(t *testing.T) {
	f := newApplyFixture(t, branchFirst)
	f.applier.Editor = nil
	f.applier.Runner = runnerFunc(func(string) (string, error) { return "", nil })
	writeFile(t, filepath.Join(f.dot, ".claude", "CLAUDE.md"), strings.Repeat("- a rule\n", textFilesBudget/8))
	plan, err := f.applier.Plan(context.Background(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready() != 0 || !strings.Contains(plan.Routes[0].Skip, "byte limit") {
		t.Errorf("route = %+v", plan.Routes[0])
	}
}

// A dry run's text-mode input shows the live files, since no worktree
// exists yet.
func TestDryRunTextModeInputShowsLiveFiles(t *testing.T) {
	f := newApplyFixture(t, branchFirst)
	f.applier.Editor = nil
	f.applier.Runner = runnerFunc(func(string) (string, error) { return "", nil })
	plan, err := f.applier.Plan(context.Background(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	in := f.applier.BuildInput(plan, PlannedWorktrees(plan))
	want := defaultMarker + " BEGIN " + dotTarget(plan) + "\n# Rules\n\n- one\n"
	if !strings.Contains(in, want) {
		t.Errorf("input misses %q:\n%s", want, in)
	}
	if _, err := os.Stat(plan.Worktrees[0]); !os.IsNotExist(err) {
		t.Error("planning created a worktree")
	}
}
