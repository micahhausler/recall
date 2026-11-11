package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/skills"
)

const skillsUsage = `Usage:
  recall skills [--since 90d] [--skills-dir DIR]... [--json]

Lists every skill it can find with how often sessions invoked it, by the
model or by slash command, and flags retirement candidates. Default skills
come from ~/.claude/skills.
`

type dirList []string

func (d *dirList) String() string     { return fmt.Sprint(*d) }
func (d *dirList) Set(v string) error { *d = append(*d, v); return nil }

func cmdSkills(args []string) int {
	fs := flag.NewFlagSet("skills", flag.ContinueOnError)
	since := fs.String("since", "90d", "window to count invocations over")
	jsonOut := fs.Bool("json", false, "print JSON instead of a table")
	var dirs dirList
	fs.Var(&dirs, "skills-dir", "an extra directory of <name>/SKILL.md (repeatable)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, skillsUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	env, err := loadArchiveEnv()
	if err != nil {
		return fail(err)
	}
	cutoff, err := parseSince(*since, env.clk.Now())
	if err != nil {
		return fail(err)
	}
	found, err := skills.Discover(append([]string{filepath.Join(env.home, ".claude", "skills")}, dirs...))
	if err != nil {
		return fail(err)
	}
	var evs []events.Event
	for _, h := range env.harnesses {
		sessions, err := h.Sessions()
		if err != nil {
			return fail(err)
		}
		for _, s := range sessions {
			se, err := h.ReadSessionFile(s, env.settings.Host)
			if err != nil {
				return fail(err)
			}
			for _, e := range se {
				if t, terr := time.Parse(time.RFC3339, e.Timestamp); terr == nil && !t.Before(cutoff) {
					evs = append(evs, e)
				}
			}
		}
	}
	rows := skills.Report(found, evs)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return fail(err)
		}
		return 0
	}
	fmt.Printf("skills found: %d; events window: since %s\n", len(found), cutoff.Format("2006-01-02"))
	skills.Print(os.Stdout, rows, cutoff)
	return 0
}
