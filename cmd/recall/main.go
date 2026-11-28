// Command recall keeps an agent's memory honest: it archives coding-agent
// transcripts, reports on skill use, lints context files, and manages the
// recurring jobs that do so.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/micahhausler/recall/internal/version"
)

// command is one top-level command. A visible one is for people and is in
// `recall --help`; a hidden one is called by the timer, a skill, a hook, or
// a script, is in `recall --help --all` only, and stays callable.
type command struct {
	name    string
	visible bool
	lines   [][2]string
	run     func([]string) int
}

var commands = []command{
	{"init", true, [][2]string{{"init [--dry-run]", "set a new machine up with defaults: no bucket, scheduler, or git remote"}}, cmdInit},
	{"sync", true, [][2]string{{"sync [--force]", "run each step that's due: imports, archive, observe, propose on the proposing machine"}}, cmdSync},
	{"report", true, [][2]string{{"report [--since 30d|all]", "say what recall noticed; no model call"}}, cmdReport},
	{"status", true, [][2]string{{"status", "what is on, the timer and its last run, the proposing machine, pending proposals"}}, cmdStatus},
	{"proposals", true, [][2]string{{"proposals", "list sent proposals and what became of them"}}, cmdProposals},
	{"config", true, [][2]string{{"config show|get|set", "read and write the layered TOML config"}}, cmdConfig},

	{"observe", false, [][2]string{{"observe [--since 30d|all]", "observe new or changed sessions, one model call each"}}, cmdObserve},
	{"archive", false, [][2]string{
		{"archive [--lookback N]", "upload each finished week missing from the store"},
		{"archive build WEEK OUT", "write one week's tarball locally"},
	}, cmdArchive},
	{"propose", false, [][2]string{{"propose [--out DIR]", "fold observations into proposals and send them for review"}}, cmdPropose},
	{"import", false, [][2]string{{"import [NAME...]", "fetch sessions through the import plugins config names"}}, cmdImport},
	{"lint", false, [][2]string{{"lint [FILES...]", "find stale paths and commands in context files"}}, cmdLint},
	{"skills", false, [][2]string{{"skills", "report which skills sessions actually invoke"}}, cmdSkills},
	{"events", false, [][2]string{{"events [PATH...]", "print normalized transcript events as JSONL"}}, cmdEvents},
	{"recur", false, [][2]string{{"recur sync|check|list|run", "reconcile declared recurring jobs with cron or launchd"}}, cmdRecur},
	{"hook", false, [][2]string{{"hook ...", "what the plugin's session hooks run"}}, cmdHook},
	{"extension", false, [][2]string{{"extension NAME", "print the owner's extension files for a skill"}}, cmdExtension},
	{"learn", false, [][2]string{{"learn ...", "the learning loop's steps under their earlier names"}}, cmdLearn},
	{"archive-chats", false, [][2]string{{"archive-chats ...", "the archive and bucket setup under their earlier names"}}, cmdArchiveChats},
}

// usage is the help text; all adds the hidden commands.
func usage(all bool) string {
	var b strings.Builder
	b.WriteString("recall keeps an agent's memory honest.\n\nUsage:\n")
	list := func(visible bool) {
		for _, c := range commands {
			if c.visible == visible {
				for _, l := range c.lines {
					fmt.Fprintf(&b, "  recall %-28s %s\n", l[0], l[1])
				}
			}
		}
	}
	list(true)
	if all {
		b.WriteString("\nCalled by the timer, skills, hooks, and scripts (docs/commands.md):\n")
		list(false)
	}
	b.WriteString("\nRun 'recall <command> -h' for a command's flags, and 'recall --version' for the release tag.\n")
	if !all {
		b.WriteString("Run 'recall --help --all' to also list the commands the timer, skills, hooks, and scripts call.\n")
	}
	return b.String()
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage(false))
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage(len(rest) == 1 && rest[0] == "--all"))
		return 0
	case "--version":
		fmt.Println("recall", version.Version)
		return 0
	}
	for _, c := range commands {
		if c.name == cmd {
			return c.run(rest)
		}
	}
	fmt.Fprintf(os.Stderr, "recall: unknown command %q\n\n%s", cmd, usage(false))
	return 2
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "recall: %v\n", err)
	return 2
}

// shortHost is `hostname -s`: the name before the first dot.
func shortHost(h string) string {
	if i := indexByte(h, '.'); i >= 0 {
		return h[:i]
	}
	return h
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
