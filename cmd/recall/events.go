package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/harness"
)

const eventsUsage = `Usage:
  recall events [--since T] [--harness NAME] [PATH...]

Prints normalized transcript events as JSONL. With no PATH, every session of
every enabled harness on this machine. A PATH is a .jsonl transcript, a
directory of them, or a week's .tar.zst archive. --since takes RFC 3339, a
date, or a window like 90d or 24h.
`

func cmdEvents(args []string) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	since := fs.String("since", "", "only events at or after this time")
	harnessName := fs.String("harness", "", "read only this harness")
	fs.Usage = func() { fmt.Fprint(os.Stderr, eventsUsage) }
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
	hs := env.harnesses
	if *harnessName != "" {
		hs = nil
		for _, h := range env.harnesses {
			if h.Name() == *harnessName {
				hs = append(hs, h)
			}
		}
		if len(hs) == 0 {
			return fail(fmt.Errorf("no enabled harness %q", *harnessName))
		}
	}

	var all []events.Event
	if fs.NArg() == 0 {
		for _, h := range hs {
			sessions, err := h.Sessions()
			if err != nil {
				return fail(err)
			}
			for _, s := range sessions {
				evs, err := h.ReadSessionFile(s, env.settings.Host)
				if err != nil {
					return fail(err)
				}
				all = append(all, evs...)
			}
		}
	}
	for _, path := range fs.Args() {
		evs, err := readPath(hs, path, env.settings.Host)
		if err != nil {
			return fail(err)
		}
		all = append(all, evs...)
	}
	if !cutoff.IsZero() {
		kept := all[:0]
		for _, e := range all {
			if t, err := time.Parse(time.RFC3339, e.Timestamp); err == nil && !t.Before(cutoff) {
				kept = append(kept, e)
			}
		}
		all = kept
	}
	if err := events.WriteJSONL(os.Stdout, all); err != nil {
		return fail(err)
	}
	return 0
}

func readPath(hs []harness.Harness, path, host string) ([]events.Event, error) {
	h := hs[0]
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	switch {
	case st.IsDir():
		var all []events.Event
		err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
				return err
			}
			evs, err := h.ReadSessionFile(p, host)
			if err != nil {
				return err
			}
			all = append(all, evs...)
			return nil
		})
		return all, err
	case strings.HasSuffix(path, ".tar.zst"):
		// An archived week names its host in the key path; the local host
		// doesn't apply to a downloaded archive.
		return harness.ReadTarZst(h, path, "")
	default:
		return h.ReadSessionFile(path, host)
	}
}

var windowRE = regexp.MustCompile(`^(\d+)([dhm])$`)

// parseSince reads RFC 3339, a date, a window like 90d, 24h, or 30m, or
// "all" for no cutoff.
func parseSince(s string, now time.Time) (time.Time, error) {
	if s == "" || s == "all" {
		return time.Time{}, nil
	}
	if m := windowRE.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute}[m[2]]
		return now.Add(-time.Duration(n) * unit), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad --since %q; want RFC 3339, a date, or a window like 90d", s)
}
