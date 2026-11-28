package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/lint"
)

const lintUsage = `Usage:
  recall lint [--skills-dir DIR]... [FILES...]
  recall lint --memory

Reads context files and the files they pull in through @imports and
Markdown links, and reports paths, commands, and skill names they mention
that don't exist on this machine. The default is every enabled harness's
context files: ~/.claude/CLAUDE.md for claude, every Markdown file under
~/.kiro/steering (AGENTS.md among them) for kiro. Exits 1 on findings.

--memory lints each harness's own memory notes instead, by the same rules,
and also reports a note its index doesn't list, an index line that points
at a missing note, and a note unchanged for longer than learn.retire_days.
It only reports; recall never edits a harness's memory.
`

func cmdLint(args []string) int {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	var dirs dirList
	fs.Var(&dirs, "skills-dir", "an extra directory of <name>/SKILL.md (repeatable)")
	memory := fs.Bool("memory", false, "lint each harness's own memory notes instead of context files")
	fs.Usage = func() { fmt.Fprint(os.Stderr, lintUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	c, err := loadConfig(config.DefaultBase(home))
	if err != nil {
		return fail(err)
	}
	hostname, _ := os.Hostname()
	settings, err := c.Settings(home, shortHost(hostname))
	if err != nil {
		return fail(err)
	}
	ignore := map[string]bool{}
	for _, p := range settings.Lint.Ignore {
		ignore[p] = true
	}
	files := fs.Args()
	if *memory && len(files) > 0 {
		fmt.Fprintln(os.Stderr, "recall: lint --memory takes no files; it reads each harness's own memory")
		return 2
	}
	env, err := loadArchiveEnv()
	if err != nil {
		return fail(err)
	}
	if len(files) == 0 && !*memory {
		for _, h := range env.harnesses {
			files = append(files, h.ContextFiles()...)
		}
		if len(files) == 0 {
			fmt.Fprintln(os.Stderr, "recall: nothing to lint: no enabled harness has a context file and no files were given")
			return 2
		}
	}
	evs, err := readEvents(env, time.Time{})
	if err != nil {
		return fail(err)
	}
	n, err := takenNames(context.Background(), env, dirs, evs)
	if err != nil {
		return fail(err)
	}
	known := map[string]bool{}
	for _, name := range n.taken() {
		known[name] = true
	}
	l := &lint.Linter{Home: home, Names: known, Ignore: ignore}
	var findings []lint.Finding
	if *memory {
		var notes []lint.Note
		for _, h := range env.harnesses {
			mems, err := h.Memories()
			if err != nil {
				return fail(err)
			}
			for _, m := range mems {
				notes = append(notes, lint.Note{Path: m.Path, Index: m.Index, Modified: m.Modified})
				files = append(files, m.Path)
			}
		}
		findings, err = l.Memory(notes, settings.Learn.RetireDays, env.clk.Now())
	} else {
		findings, err = l.Run(files)
	}
	if err != nil {
		return fail(err)
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(findings) > 0 {
		return 1
	}
	fmt.Fprintf(os.Stderr, "recall lint: %d file(s) clean\n", len(files))
	return 0
}
