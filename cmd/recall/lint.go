package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/micahhausler/recall/internal/lint"
	"github.com/micahhausler/recall/internal/skills"
)

const lintUsage = `Usage:
  recall lint [--skills-dir DIR]... [FILES...]

Reads context files (default ~/.claude/CLAUDE.md and its @imports) and
reports paths, commands, and skill names they mention that don't exist on
this machine. Exits 1 on findings.
`

func cmdLint(args []string) int {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	var dirs dirList
	fs.Var(&dirs, "skills-dir", "an extra directory of <name>/SKILL.md (repeatable)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, lintUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	files := fs.Args()
	if len(files) == 0 {
		def := filepath.Join(home, ".claude", "CLAUDE.md")
		if _, err := os.Stat(def); err != nil {
			fmt.Fprintf(os.Stderr, "recall: nothing to lint: %s doesn't exist and no files were given\n", def)
			return 2
		}
		files = []string{def}
	}
	found, err := skills.Discover(append([]string{filepath.Join(home, ".claude", "skills")}, dirs...))
	if err != nil {
		return fail(err)
	}
	known := map[string]bool{}
	for _, s := range found {
		known[s.Name] = true
	}
	l := &lint.Linter{Home: home, Skills: known}
	findings, err := l.Run(files)
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
