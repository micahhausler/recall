// Command recall keeps an agent's memory honest: it archives coding-agent
// transcripts, reports on skill use, lints context files, and manages the
// recurring jobs that do so.
package main

import (
	"fmt"
	"os"
)

const usage = `recall keeps an agent's memory honest.

Usage:
  recall config show|get|set       read and write the layered TOML config
  recall recur sync|check|list|run reconcile declared recurring jobs with cron or launchd
  recall archive-chats ...         upload each week's transcripts to the store
  recall events [PATH...]          print normalized transcript events as JSONL
  recall skills                    report which skills sessions actually invoke
  recall lint [FILES...]           find stale paths and commands in context files

Run 'recall <command> -h' for a command's flags.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	case "config":
		return cmdConfig(rest)
	case "recur":
		return cmdRecur(rest)
	case "archive-chats":
		return cmdArchiveChats(rest)
	case "events":
		return cmdEvents(rest)
	case "skills":
		return cmdSkills(rest)
	case "lint":
		return cmdLint(rest)
	default:
		fmt.Fprintf(os.Stderr, "recall: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
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
