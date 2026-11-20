package lint

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, text string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(t *testing.T, l *Linter, files ...string) []string {
	t.Helper()
	fs, err := l.Run(files)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range fs {
		out = append(out, f.String())
	}
	return out
}

func linter(t *testing.T, home string, bins ...string) *Linter {
	onPath := map[string]bool{}
	for _, b := range bins {
		onPath[b] = true
	}
	return &Linter{
		Home:  home,
		Names: map[string]bool{"recur": true, "archive-chats": true, "help": true},
		LookPath: func(name string) (string, error) {
			if onPath[name] {
				return "/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
	}
}

func TestLintFindsStaleMentions(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "exists.md"), "x")
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	main := write(t, filepath.Join(home, ".claude", "CLAUDE.md"), strings.Join([]string{
		"Read `~/exists.md` before starting.",       // exists: no finding
		"Read `~/missing.md` when you can.",         // finding
		"Run `jobs-sync --check` after.",            // not on PATH: finding
		"Run `chat-archive run` weekly.",            // on PATH: no finding
		"Use `/recur` or `/archive-chats`.",         // skills exist
		"Use `/distill-prompts` for the loop.",      // finding
		"`/help` is a builtin.",                     // no finding
		"Keep `one sentence per line` in Markdown.", // prose: never checked
		"Globs like `~/notes/*.md` are skipped.",
		"Run `go test ./...` before pushing.",    // plain word command: skipped
		"Write scratch files under `/tmp`.",      // a path that exists, not a skill
		"Pass `-p PackageName -p Other` to it.",  // a flag, not a command
		"Run from `/path/to/workspace/src/pkg`.", // the docs placeholder
	}, "\n")+"\n")
	got := run(t, linter(t, home, "chat-archive"), main)
	want := []string{
		"CLAUDE.md:2: path ~/missing.md doesn't exist",
		"CLAUDE.md:3: command jobs-sync not on PATH",
		`CLAUDE.md:6: skill /distill-prompts: no skill or command named "distill-prompts" on this machine`,
	}
	if len(got) != len(want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
	for i := range want {
		if !strings.HasSuffix(got[i], want[i][strings.Index(want[i], ":"):]) && !strings.Contains(got[i], want[i]) {
			t.Errorf("finding %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLintFollowsImports(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	imported := write(t, filepath.Join(dir, "work.md"), "Check `~/gone.md` daily.\n")
	main := write(t, filepath.Join(dir, "CLAUDE.md"), "@work.md\n@missing.md\n")
	got := run(t, linter(t, home), main)
	if len(got) != 2 {
		t.Fatalf("findings = %v", got)
	}
	if !strings.Contains(got[0], "missing.md doesn't exist") {
		t.Errorf("got[0] = %q", got[0])
	}
	if !strings.Contains(got[1], imported) || !strings.Contains(got[1], "gone.md") {
		t.Errorf("got[1] = %q", got[1])
	}
}

func TestLintImportCycleTerminates(t *testing.T) {
	home := t.TempDir()
	a := filepath.Join(home, "a.md")
	b := filepath.Join(home, "b.md")
	write(t, a, "@b.md\n")
	write(t, b, "@a.md\n")
	if got := run(t, linter(t, home), a); len(got) != 0 {
		t.Errorf("findings = %v", got)
	}
}

func TestLintNoSkillsMeansNoSkillChecks(t *testing.T) {
	home := t.TempDir()
	main := write(t, filepath.Join(home, "CLAUDE.md"), "Use `/whatever`.\n")
	l := linter(t, home)
	l.Names = nil
	if got := run(t, l, main); len(got) != 0 {
		t.Errorf("findings = %v; with no skills discovered, skill checks are skipped", got)
	}
}

func TestLintIgnoredPaths(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	main := write(t, filepath.Join(dir, "CLAUDE.md"), "@home.md\nRead `~/gone.md` daily.\n")
	l := linter(t, home)
	l.Ignore = map[string]bool{
		filepath.Join(dir, "home.md"):  true,
		filepath.Join(home, "gone.md"): true,
	}
	if got := run(t, l, main); len(got) != 0 {
		t.Errorf("findings = %v; ignored paths must not be reported", got)
	}
	// A missing path not on the list still reports.
	l.Ignore = map[string]bool{filepath.Join(dir, "home.md"): true}
	if got := run(t, l, main); len(got) != 1 || !strings.Contains(got[0], "gone.md") {
		t.Errorf("findings = %v, want only gone.md", got)
	}
}

// AGENTS.md points at more context with Markdown links, not @imports.
func TestLintFollowsMarkdownLinks(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "docs", "build.md"), "Read `~/gone.md` first.\n")
	write(t, filepath.Join(home, "notes.txt"), "x")
	agents := write(t, filepath.Join(home, "AGENTS.md"), strings.Join([]string{
		"See [the build guide](docs/build.md#steps) and [notes](notes.txt \"Notes\").", // followed; exists
		"See [old](docs/old.md).",                               // finding
		"See [site](https://example.com/x.md) and [top](#top).", // not local: skipped
		"Not a link: `[x](missing.md)`.",                        // backticked: skipped
	}, "\n"))
	got := run(t, linter(t, home), agents)
	want := []string{
		agents + ":2: link docs/old.md: " + filepath.Join(home, "docs", "old.md") + " doesn't exist",
		filepath.Join(home, "docs", "build.md") + ":1: path ~/gone.md doesn't exist",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestLintMemory(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	index := write("MEMORY.md", "- [Listed](listed.md) — fine\n- [Gone](gone.md) — deleted since\n")
	listed := write("listed.md", "Run `~/bin/missing-script` first.\n")
	unlisted := write("unlisted.md", "Nothing stale in here.\n")
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	notes := []Note{
		{Path: listed, Index: index, Modified: now.AddDate(0, 0, -10)},
		{Path: unlisted, Index: index, Modified: now.AddDate(0, 0, -200)},
	}
	l := &Linter{Home: home, LookPath: func(string) (string, error) { return "", os.ErrNotExist }}
	findings, err := l.Memory(notes, 90, now)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range findings {
		got = append(got, filepath.Base(f.File)+": "+f.Message)
	}
	want := []string{
		"listed.md: path ~/bin/missing-script doesn't exist",
		"MEMORY.md: index points at gone.md, which doesn't exist",
		"unlisted.md: not listed in MEMORY.md, so the harness never loads it",
		"unlisted.md: last changed 200 days ago; nothing has kept it current",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
