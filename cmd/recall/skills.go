package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/skills"
)

const skillsUsage = `Usage:
  recall skills [--since 90d] [--skills-dir DIR]... [--json]
  recall skills --taken      every skill and command name already in use

Lists every skill it can find with how often sessions invoked it, by the
model or by slash command, and flags retirement candidates. The skill list
is the harness's own, as a session started in your home directory sees it
(plugin skills as plugin:skill), plus any --skills-dir. A project skill
counts as found when the transcript recorded where it loaded from.
`

type dirList []string

func (d *dirList) String() string     { return fmt.Sprint(*d) }
func (d *dirList) Set(v string) error { *d = append(*d, v); return nil }

func cmdSkills(args []string) int {
	fs := flag.NewFlagSet("skills", flag.ContinueOnError)
	since := fs.String("since", "90d", "window to count invocations over")
	jsonOut := fs.Bool("json", false, "print JSON instead of a table")
	takenOut := fs.Bool("taken", false, "print every skill and slash command name already taken, one per line")
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
	evs, err := readEvents(env, cutoff)
	if err != nil {
		return fail(err)
	}
	n, err := takenNames(context.Background(), env, dirs, evs)
	if err != nil {
		return fail(err)
	}
	if *takenOut {
		for _, name := range n.taken() {
			fmt.Println(name)
		}
		return 0
	}
	found := n.skills
	rows := skills.Report(found, n.commands, evs)
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

// names is what's taken on this machine: the skills each harness reports
// for a session started in home, any extra skill directories, and every
// slash command a harness accepts. Commands are the harness's own startup
// list plus the built-ins sessions actually ran, since a headless session
// doesn't list interactive-only commands like /resume.
type names struct {
	skills   []skills.Skill
	commands map[string]bool
}

func (n names) taken() []string {
	set := map[string]bool{}
	for _, s := range n.skills {
		set[s.Name] = true
	}
	for c := range n.commands {
		set[c] = true
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func takenNames(ctx context.Context, env *archiveEnv, extra []string, evs []events.Event) (names, error) {
	n := names{commands: map[string]bool{}}
	found, err := skills.Discover(extra)
	if err != nil {
		return n, err
	}
	n.skills = found
	for _, h := range env.harnesses {
		inv, err := h.Inventory(ctx, env.home)
		if err != nil {
			return n, fmt.Errorf("%s inventory: %w", h.Name(), err)
		}
		for _, name := range inv.Skills {
			n.skills = append(n.skills, skills.Skill{Name: name})
		}
		for _, c := range inv.Commands {
			n.commands[c] = true
		}
	}
	for _, e := range evs {
		if e.Kind == events.BuiltinCommand && e.Skill != "" {
			n.commands[e.Skill] = true
		}
	}
	return n, nil
}

// readEvents reads every enabled harness's sessions on this machine, keeping
// events at or after cutoff (all of them for a zero cutoff).
func readEvents(env *archiveEnv, cutoff time.Time) ([]events.Event, error) {
	var evs []events.Event
	for _, h := range env.harnesses {
		sessions, err := h.Sessions()
		if err != nil {
			return nil, err
		}
		for _, s := range sessions {
			se, err := h.ReadSessionFile(s, env.settings.Host)
			if err != nil {
				return nil, err
			}
			for _, e := range se {
				if cutoff.IsZero() {
					evs = append(evs, e)
				} else if t, terr := time.Parse(time.RFC3339, e.Timestamp); terr == nil && !t.Before(cutoff) {
					evs = append(evs, e)
				}
			}
		}
	}
	return evs, nil
}
