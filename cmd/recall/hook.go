package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/hook"
	"github.com/micahhausler/recall/internal/learn"
)

const hookUsage = `Usage:
  recall hook session-start    print the repo note for the repo a session starts in
  recall hook session-end      start the loop's background work, and return at once

The recall plugin installs these as Claude Code's SessionStart and
SessionEnd hooks.

session-start reads only local files: with learn.repo_notes set, it adds the
owner's repo note for the repo the session starts in to the session's context,
and prints nothing otherwise.

session-end runs the learning loop with no cron job or LaunchAgent. The
background work does only what [hooks] in the config turns on: hooks.map,
hooks.archive, and hooks.learn_every. Its output is in
~/.local/state/recall/hook.log.
`

// Background work is one at a time, and a worker that hasn't finished in
// workerStale is taken for dead. Sessions are mapped once they have been
// quiet for mapIdle: long enough that one picked up again the same day is
// mapped once, not every time it ends.
const (
	workerStale = 2 * time.Hour
	mapIdle     = 6 * time.Hour
	retryGap    = 24 * time.Hour
)

func hookStateDir(env *archiveEnv) string {
	return filepath.Join(env.home, ".local", "state", "recall")
}

func cmdHook(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, hookUsage)
		return 2
	}
	// A hook must never fail or slow a session: every path below answers
	// 0, and says what went wrong only on stderr or in the log.
	if hook.Guarded() && args[0] != "work" {
		return 0 // one of recall's own model calls; leave it alone
	}
	env, err := loadArchiveEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: %v\n", err)
		return 0
	}
	switch args[0] {
	case "session-start":
		if err := sessionStart(hook.ReadInput(os.Stdin), env, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "recall: %v\n", err)
		}
		return 0
	case "session-end":
		hook.ReadInput(os.Stdin)
		h := env.settings.Hooks
		if !h.Map && !h.Archive && h.LearnEvery <= 0 {
			return 0
		}
		if err := startWorker(env); err != nil {
			fmt.Fprintf(os.Stderr, "recall: %v\n", err)
		}
		return 0
	case "work":
		hookWork(env)
		return 0
	default:
		fmt.Fprint(os.Stderr, hookUsage)
		return 2
	}
}

// sessionStart writes the SessionStart hook's answer: the repo note for the
// repo holding the session's directory as added context, or nothing.
func sessionStart(in hook.Input, env *archiveEnv, w io.Writer) error {
	text, err := learn.RepoNoteContext(repoNotesDir(env), in.CWD, env.home)
	if err != nil || text == "" {
		return err
	}
	out := map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName":     "SessionStart",
		"additionalContext": text,
	}}
	return json.NewEncoder(w).Encode(out)
}

// startWorker runs `recall hook work` detached, in its own session with its
// output in the hook log, so it outlives the harness session that ended.
func startWorker(env *archiveEnv) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir := hookStateDir(env)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(dir, "hook.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(exe, "hook", "work")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// hookWork is the background worker: the steps [hooks] turns on, in the
// order a scheduled machine would run them, under one lock. A step an
// enabled recur job's command already runs is skipped, judged by the
// step's command, so a machine that schedules part of the loop never runs
// that part twice. Its standard output and error are the hook log.
func hookWork(env *archiveEnv) {
	dir := hookStateDir(env)
	now := env.clk.Now()
	release, ok := hook.Lock(filepath.Join(dir, "hook.lock"), workerStale, now)
	if !ok {
		return // another session's worker is on it
	}
	defer release()
	// The worker stops before its lock goes stale, so a hung model call
	// can never leave two workers running.
	ctx, cancel := context.WithTimeout(context.Background(), workerStale-10*time.Minute)
	defer cancel()
	h := env.settings.Hooks
	jobs := env.settings.Recur
	stamp := func(name string) string { return filepath.Join(dir, "hook-"+name+".stamp") }
	fmt.Printf("--- %s recall hook work\n", now.Format(time.RFC3339))
	// Each periodic step is marked as tried before it runs. One that keeps
	// failing is then tried once a day, not at the end of every session,
	// which for the reduce would be a paid model call each time.
	if h.Archive && !jobs.Covers(config.StepArchive) && hook.Due(stamp("archive"), retryGap, now) {
		_ = hook.Touch(stamp("archive"), now)
		archiveRun(ctx, env, 0, false, false)
	}
	doMap := h.Map && !jobs.Covers(config.StepMap)
	doLearn := h.LearnEvery > 0 && !jobs.Covers(config.StepLearn)
	runner, rerr := env.newRunner()
	if rerr != nil && (doMap || doLearn) {
		fmt.Fprintf(os.Stderr, "recall: %v\n", rerr)
		return
	}
	if doMap {
		// The first run reaches back over the transcripts still on disk,
		// bounded by the same window `recall init` prints for the map, so
		// the first proposal arrives in days, not after a week of new
		// sessions. Later runs carry on from where the last one left off.
		since := hook.Since(filepath.Join(dir, "hook-map.since"), now.AddDate(0, 0, -mapWindowDays))
		if oldest := now.AddDate(0, 0, -mapWindowDays); since.Before(oldest) {
			since = oldest
		}
		// Evidence sources are scanned at most once a day, like the archive
		// step: a scan is dozens of subprocess calls per repo, too many to
		// repeat at the end of every session.
		evidenceGate := func(name string) bool {
			s := stamp("evidence-" + name)
			if !hook.Due(s, retryGap, now) {
				return false
			}
			_ = hook.Touch(s, now)
			return true
		}
		learnMap(ctx, env, runner, since.UTC().Format(time.RFC3339), 0, false, false, mapIdle, evidenceGate)
	}
	every := time.Duration(h.LearnEvery) * 24 * time.Hour
	if doLearn && hook.Due(stamp("learn"), every, now) && hook.Due(stamp("learn-tried"), retryGap, now) {
		_ = hook.Touch(stamp("learn-tried"), now)
		// Done only when the reduce had candidates to read and its proposals
		// applied or there was nothing to apply: a reduce over nothing must
		// not push the first real one learn_every days out, and a run whose
		// proposals were dropped must not count as the period's pass.
		if code, done := learnRun(ctx, env, runner, "", reduceWindow{}, false); code == 0 && done {
			_ = hook.Touch(stamp("learn"), now)
		}
	}
}
