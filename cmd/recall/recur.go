package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/recur"
)

const recurUsage = `Usage:
  recall recur sync         reconcile declared jobs with cron or launchd
  recall recur check        change nothing; list each job as installed and what
                            a sync would change; exit 1 on drift
  recall recur list         show the declared jobs and which file each comes from
  recall recur run NAME     run one job now, as the scheduler would
  --quiet                   with sync: print only changes, never fail
`

func cmdRecur(args []string) int {
	fs := flag.NewFlagSet("recur", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "print only changes, never fail")
	scheduler := fs.String("scheduler", recur.ForOS(), "cron or launchd (testing)")
	self := fs.String("self", "", "binary path written into schedules (testing)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, recurUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	cmd := "sync"
	if len(rest) > 0 {
		cmd = rest[0]
	}

	rc, err := recurMain(cmd, rest, *quiet, *scheduler, *self)
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: %v\n", err)
		if *quiet {
			return 0
		}
		return 2
	}
	return rc
}

func recurMain(cmd string, rest []string, quiet bool, scheduler, self string) (int, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return 2, err
	}
	c, err := config.Load(config.DefaultBase(home))
	if err != nil {
		return 2, err
	}
	hostname, _ := os.Hostname()
	settings, err := c.Settings(home, shortHost(hostname))
	if err != nil {
		return 2, err
	}
	if self == "" {
		self, err = os.Executable()
		if err != nil {
			return 2, err
		}
	}
	setup, err := recur.LoadSetup(settings, self, os.Getenv("RECALL_CONFIG"), home)
	if err != nil {
		return 2, err
	}
	jobs := settings.Recur.Jobs

	switch cmd {
	case "run":
		if len(rest) != 2 {
			return 2, fmt.Errorf("run needs a job name")
		}
		var echo io.Writer
		if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			echo = os.Stdout
		}
		rc, err := setup.Run(rest[1], jobs, echo)
		if err != nil {
			return rc, err
		}
		return rc, nil
	case "list":
		fmt.Printf("config: %s%s\n", tilde(c.Base, home), missingNote(c.Base))
		fmt.Printf("prefix: %s\n", setup.Prefix)
		if setup.CronTZ != "" {
			fmt.Printf("crontz: %s\n", setup.CronTZ)
		}
		for _, e := range settings.Recur.Env {
			fmt.Printf("env:    %s\n", e)
		}
		for _, name := range sortedNames(jobs) {
			j := jobs[name]
			fmt.Printf("%-20s %-16s %s  (%s)\n", j.Name, j.Schedule, j.Command, tilde(j.Source.File, home))
		}
		return 0, nil
	case "sync", "check":
		check := cmd == "check"
		sched, err := recur.New(scheduler, setup)
		if err != nil {
			return 2, err
		}
		plan, err := sched.Sync(jobs, check, os.Stdout)
		if err != nil {
			return 2, err
		}
		if check {
			if len(jobs) == 0 && len(plan) == 0 {
				fmt.Println("recall recur: no jobs declared")
			} else if len(plan) == 0 {
				fmt.Println("recall recur: up to date")
			}
			if len(plan) > 0 {
				return 1, nil
			}
			return 0, nil
		}
		if len(plan) == 0 && !quiet {
			fmt.Println("recall recur: up to date")
		}
		return 0, nil
	default:
		fmt.Fprint(os.Stderr, recurUsage)
		return 2, nil
	}
}

func sortedNames(jobs map[string]config.Job) []string {
	names := make([]string, 0, len(jobs))
	for n := range jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func missingNote(path string) string {
	if _, err := os.Stat(path); err != nil {
		return " (missing)"
	}
	return ""
}
