package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/learn"
	"github.com/micahhausler/recall/internal/recur"
	"github.com/micahhausler/recall/internal/review"
	"github.com/micahhausler/recall/internal/store"
)

const statusUsage = `Usage:
  recall status    what is on, the timer and its last run, the proposing machine,
                   and pending proposals; changes nothing and calls no model
`

// statusDeps are what status reads besides the settings; tests swap them.
type statusDeps struct {
	base      string
	scheduler string
	self      string
	loc       *time.Location
	// openStore returns nil, nil for a store that doesn't exist yet.
	openStore func(context.Context, *archiveEnv) (store.Store, error)
}

func cmdStatus(args []string) int {
	if len(args) > 0 {
		fmt.Fprint(os.Stderr, statusUsage)
		if args[0] == "-h" || args[0] == "--help" {
			return 0
		}
		return 2
	}
	env, err := loadArchiveEnv()
	if err != nil {
		home, _ := os.UserHomeDir()
		fmt.Fprintf(os.Stderr, "recall: %s\n", scrubStatus(err.Error(), home))
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	deps := statusDeps{
		base:      config.DefaultBase(env.home),
		scheduler: recur.ForOS(),
		self:      self,
		loc:       time.Local,
		openStore: func(ctx context.Context, env *archiveEnv) (store.Store, error) {
			return initStore(ctx, env, true)
		},
	}
	writeStatus(context.Background(), os.Stdout, env, deps)
	return 0
}

// writeStatus prints the report; every failure is a line, never a stop.
func writeStatus(ctx context.Context, w io.Writer, env *archiveEnv, d statusDeps) {
	s := env.settings
	line := func(label, format string, a ...any) {
		fmt.Fprintf(w, "%-10s %s\n", label, fmt.Sprintf(format, a...))
	}
	more := func(format string, a ...any) { line("", format, a...) }
	clean := func(err error) string {
		return scrubStatus(strings.SplitN(err.Error(), "\n", 2)[0], env.home)
	}

	line("config:", "%s%s", tilde(d.base, env.home), missingNote(d.base))
	line("host:", "%s", s.Host)

	// The first store read decides reachability; after a failure nothing
	// else asks the store.
	var st store.Store
	var reducer, storeDown string
	where := tilde(s.Store.Path, env.home)
	if s.Store.Kind == "s3" {
		where = "s3://" + s.Store.S3.Bucket
		if r := s.Store.S3.Region; r != "" {
			where += " (" + r + ")"
		}
	}
	var err error
	if d.openStore != nil {
		st, err = d.openStore(ctx, env)
	}
	if err == nil && st != nil {
		reducer, err = storeReducer(ctx, st)
	}
	switch {
	case err != nil:
		st, storeDown = nil, "unreachable"
		where += "; unreachable: " + clean(err)
	case st == nil && s.Store.Kind == "fs":
		where += " (not created yet)"
	}
	line("store:", "%s, %s", s.Store.Kind, where)

	if names := env.harnessNames(); len(names) == 0 {
		line("agents:", "none enabled")
	} else {
		line("agents:", "%s", strings.Join(names, ", "))
		name := "<name>"
		if len(names) == 1 {
			name = names[0]
		}
		more("off: recall config set harness.%s.enabled false", name)
	}

	switch {
	case storeDown != "":
		line("proposing:", "unknown; the store is %s", storeDown)
	case reducer == "":
		line("proposing:", "none yet; recall init --claim makes this machine the proposing one")
	case reducer == s.Host:
		line("proposing:", "this machine")
	default:
		line("proposing:", "%s, not this machine", reducer)
	}

	jobs := s.Recur.Jobs
	setup, lerr := recur.LoadSetup(s, d.self, os.Getenv(config.EnvVar), env.home)
	err = lerr
	var sched recur.Scheduler
	if err == nil {
		sched, err = recur.New(d.scheduler, setup)
	}
	if err != nil {
		line("timer:", "%s: %s", d.scheduler, clean(err))
	} else if plan, err := sched.Sync(jobs, true, io.Discard); err != nil {
		line("timer:", "%s, unreadable: %s", d.scheduler, clean(err))
	} else if len(plan) > 0 {
		line("timer:", "%s, out of date (%d %s); recall recur sync installs it", d.scheduler, len(plan), plural(len(plan), "change", "changes"))
	} else if len(jobs) == 0 {
		line("timer:", "none")
	} else {
		line("timer:", "%s, %d %s installed", d.scheduler, len(jobs), plural(len(jobs), "job", "jobs"))
	}

	h := s.Hooks
	hookLoop := config.HookRunsLoop(s.Harnesses)
	steps := []struct {
		name, step, stamp, offKey, offValue string
		hookOn                              bool
	}{
		{"archive", config.StepArchive, "archive", "hooks.archive", "false", h.Archive},
		{"observe", config.StepMap, "map", "hooks.map", "false", h.Map},
		{"propose", config.StepLearn, "learn", "hooks.learn_every", "0", h.LearnEvery > 0},
	}
	for _, step := range steps {
		label := step.name + ":"
		hookOff := fmt.Sprintf("recall config set %s %s", step.offKey, step.offValue)
		names := coveringJobs(jobs, step.step)
		switch {
		case len(names) > 0:
			last := "never run"
			if when, note, ran := lastRun(env, runSource{job: names[0]}); ran {
				last = "last run " + when.In(d.loc).Format("2006-01-02 15:04") + note
			}
			line(label, "timer job %s (%s), %s", names[0], jobs[names[0]].Schedule, last)
			var off []string
			for _, n := range names {
				off = append(off, "recall config set recur.jobs."+n+".enabled false")
			}
			// The hook takes the step back once no job runs it.
			if step.hookOn {
				off = append(off, hookOff)
			}
			more("off: %s && recall recur sync", strings.Join(off, " && "))
			for _, n := range names {
				var also []string
				for _, other := range steps {
					if other.step != step.step && config.RunsStep(jobs[n].Command, other.step) {
						also = append(also, other.name)
					}
				}
				if len(also) > 0 {
					more("turning off job %s also turns off %s", n, strings.Join(also, " and "))
				}
			}
		case step.hookOn && !hookLoop:
			line(label, "not running; %s is on, but no enabled agent's session-end hook runs the loop", step.offKey)
		case step.hookOn:
			// Hook stamps mark a try, not a finish.
			last := "never run"
			if when, _, ran := lastRun(env, runSource{hookStep: step.stamp}); ran {
				last = "last tried " + when.In(d.loc).Format("2006-01-02 15:04")
			}
			line(label, "session-end hook, %s", last)
			more("off: %s", hookOff)
		default:
			line(label, "off")
		}
	}

	if when, note, ran := lastRun(env, runSource{syncStep: syncAll}); ran {
		line("sync:", "last run %s%s", when.In(d.loc).Format("2006-01-02 15:04"), note)
	} else {
		line("sync:", "never run; recall sync runs each step that's due")
	}
	if names := importNames(env); len(names) > 0 {
		line("imports:", "run by recall sync on the proposing machine")
		for _, name := range names {
			switch {
			case storeDown != "":
				more("%s: unknown; the store is %s", name, storeDown)
			case reducer == "":
				more("%s: skipped here; no machine proposes for this store yet, and recall init --claim makes this one the proposing machine", name)
			case reducer != s.Host:
				more("%s: run by %s, skipped here", name, reducer)
			default:
				if when, note, ran := lastRun(env, runSource{syncStep: "import " + name}); ran {
					more("%s: last run %s%s", name, when.In(d.loc).Format("2006-01-02 15:04"), note)
				} else {
					more("%s: never run by recall sync", name)
				}
			}
		}
	}

	n, err := pendingProposals(ctx, st, config.LedgerPath(env.home), s.Host, reducer)
	switch {
	case err != nil:
		line("proposals:", "unknown: %s", clean(err))
	case storeDown != "" && n == 0:
		line("proposals:", "unknown; the store is %s", storeDown)
	case storeDown != "":
		line("proposals:", "%d pending in this machine's copy; the store is %s", n, storeDown)
	case n == 0:
		line("proposals:", "none pending")
	default:
		line("proposals:", "%d pending; recall proposals lists them", n)
	}
	if err := waitingGate(env); err != nil {
		line("waiting:", "%s", clean(err))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// coveringJobs are the declared jobs, by name, whose command runs step.
func coveringJobs(jobs map[string]config.Job, step string) []string {
	var out []string
	for _, name := range sortedNames(jobs) {
		if config.RunsStep(jobs[name].Command, step) {
			out = append(out, name)
		}
	}
	return out
}

// runSource is where a step's last run is recorded: a recur job's log, a
// session-end hook stamp, or a step of the last `recall sync`.
type runSource struct {
	job, hookStep, syncStep string
}

// syncAll is the whole last sync, as a syncStep.
const syncAll = "*"

// lastRun is the one place status learns when a step last ran.
func lastRun(env *archiveEnv, src runSource) (when time.Time, note string, ok bool) {
	switch {
	case src.job != "":
		return lastJobRun(filepath.Join(recur.StateDir(env.home), src.job+".log"))
	case src.syncStep != "":
		st, ok := loadSyncState(config.SyncStatePath(env.home))
		if !ok {
			return time.Time{}, "", false
		}
		if src.syncStep == syncAll {
			var parts []string
			for _, step := range st.Steps {
				parts = append(parts, step.Step+" "+step.Result)
			}
			return st.Started, ": " + strings.Join(parts, ", "), true
		}
		for _, step := range st.Steps {
			if step.Step == src.syncStep {
				return step.Time, ", " + step.Result, true
			}
		}
		return time.Time{}, "", false
	}
	hookStep := src.hookStep
	dir := hookStateDir(env)
	switch hookStep {
	case "map":
		// The worker's banner; the map has no stamp of its own.
		for _, l := range tailLines(filepath.Join(dir, "hook.log")) {
			if f := strings.Fields(l); len(f) >= 4 && f[0] == "---" && f[2] == "recall" && f[3] == "hook" {
				if t, err := time.Parse(time.RFC3339, f[1]); err == nil {
					when, ok = t, true
				}
			}
		}
		return when, "", ok
	case "learn":
		hookStep = "learn-tried"
	}
	fi, err := os.Stat(filepath.Join(dir, "hook-"+hookStep+".stamp"))
	if err != nil {
		return time.Time{}, "", false
	}
	return fi.ModTime(), "", true
}

// lastJobRun reads the newest start marker in a job's log and its exit.
func lastJobRun(path string) (when time.Time, note string, ok bool) {
	for _, l := range tailLines(path) {
		f := strings.Fields(l)
		if len(f) < 4 || f[0] != "===" {
			continue
		}
		t, err := time.Parse(time.RFC3339, f[1])
		if err != nil {
			continue
		}
		switch f[2] {
		case "start":
			when, note, ok = t, ", no exit recorded", true
		case "exit":
			if ok {
				if _, err := strconv.Atoi(f[3]); err == nil {
					note = ", exit " + f[3]
				}
			}
		}
	}
	return when, note, ok
}

// tailLines is the last 64 KiB of a file, as lines.
func tailLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const max = 64 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		if _, err := f.Seek(fi.Size()-max, io.SeekStart); err != nil {
			return nil
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}

// pendingProposals counts pending reviews on the ledger a sync would load,
// or on the local copy with no store.
func pendingProposals(ctx context.Context, st store.Store, local, host, reducer string) (int, error) {
	var l *learn.Ledger
	var err error
	if st != nil {
		l, err = learn.PeekLedger(ctx, st, local, host, reducer)
	} else {
		l, err = learn.LoadLedger(local)
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rv := range l.Reviews {
		if rv.Verdict == review.Pending {
			n++
		}
	}
	return n, nil
}

var (
	arnRE     = regexp.MustCompile(`arn:aws[a-z-]*:\S+`)
	accountRE = regexp.MustCompile(`\b[0-9]{12}\b`)
)

// scrubStatus keeps ARNs, account IDs, and the home directory out of a line.
func scrubStatus(msg, home string) string {
	msg = arnRE.ReplaceAllString(msg, "<arn>")
	msg = accountRE.ReplaceAllString(msg, "<account>")
	home = filepath.Clean(home)
	if home == "/" || home == "." {
		return msg
	}
	var b strings.Builder
	for {
		i := strings.Index(msg, home)
		if i < 0 {
			b.WriteString(msg)
			return b.String()
		}
		end := i + len(home)
		whole := (i == 0 || !pathByte(msg[i-1])) && (end == len(msg) || msg[end] == '/' || !pathByte(msg[end]))
		b.WriteString(msg[:i])
		if whole {
			b.WriteString("~")
		} else {
			b.WriteString(home)
		}
		msg = msg[end:]
	}
}

// pathByte is a byte that continues a path element.
func pathByte(c byte) bool {
	return c == '/' || c == '.' || c == '-' || c == '_' || c == '@' || c == '+' || c == '~' ||
		'0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}
