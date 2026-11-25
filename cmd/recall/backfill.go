package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/hook"
)

// The answers `recall init --backfill` takes.
const (
	backfillAsk = "ask"
	backfillYes = "yes"
	backfillNo  = "no"
)

// initBackfill offers to map the last mapWindowDays of sessions now, and on
// the store's reducer to run the loop right after, so the first findings
// arrive during init rather than after hooks.learn_every days. mode is
// backfillAsk, backfillYes, or backfillNo; an answer other than yes, end of
// input included, declines. It reports whether the map ran.
func initBackfill(ctx context.Context, env *archiveEnv, in io.Reader, mode string) bool {
	if mode == backfillNo {
		return false
	}
	if env.runner == nil {
		if bin := config.DefaultCLI(env.settings.Learn.Runner); bin != "" {
			if _, err := exec.LookPath(bin); err != nil {
				return false // init already warned it isn't on PATH
			}
		}
	}
	runner, err := env.newRunner()
	if err != nil {
		fmt.Printf("backfill: %v\n", err)
		return false
	}
	p, err := planMap(ctx, env, runner, fmt.Sprintf("%dd", mapWindowDays), 0, false, 0)
	if err != nil {
		fmt.Printf("backfill: %v\n", err)
		return false
	}
	observe, judge := p.calls(ctx)
	if observe+judge == 0 {
		fmt.Printf("\nbackfill: no session from the last %d days needs mapping\n", mapWindowDays)
		return false
	}
	reduces := reducesHere(ctx, env)
	fmt.Printf("\nbackfill: %d session(s) from the last %d days are unmapped; mapping them is %s.\n",
		observe+judge, mapWindowDays, mapEstimate(env.settings, observe+judge))
	if reduces {
		fmt.Println("backfill: this machine is the store's reducer, so the reduce and apply run right after, instead of in hooks.learn_every days.")
	} else {
		fmt.Println("backfill: another machine reduces for this store, so this one maps only.")
	}
	if mode == backfillAsk {
		fmt.Print("Map them now? [Y/n] ")
		line, err := bufio.NewReader(in).ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		if err != nil && answer == "" {
			fmt.Println()
			answer = "n" // no input to read: a script, not a yes
		}
		if answer != "" && answer != "y" && answer != "yes" {
			fmt.Println("backfill: skipped")
			return false
		}
	}
	// One map at a time: a session ending now starts the hook worker, which
	// would map the same sessions.
	dir := hookStateDir(env)
	release, ok := hook.Lock(filepath.Join(dir, "hook.lock"), workerStale, env.clk.Now())
	if !ok {
		fmt.Println("backfill: the session hook's worker is running; it maps these sessions instead")
		return false
	}
	defer release()
	start := env.clk.Now()
	if code := p.execute(ctx, nil, nil); code != 0 {
		fmt.Fprintln(os.Stderr, "recall: backfill: the map didn't finish; the session hook retries what it missed")
	}
	if reduces {
		code, done := learnRun(ctx, env, runner, "", false)
		h := env.settings.Hooks
		if code == 0 && done && h.LearnEvery > 0 && !env.settings.Recur.Covers(config.StepLearn) {
			// The period's pass ran here; the hook's next one is due
			// hooks.learn_every days from now.
			_ = hook.Touch(filepath.Join(dir, "hook-learn.stamp"), env.clk.Now())
		}
	}
	fmt.Printf("backfill: done in %s\n", env.clk.Now().Sub(start).Round(time.Second))
	return true
}

// reducesHere reports whether this machine holds the store's reduce claim.
func reducesHere(ctx context.Context, env *archiveEnv) bool {
	st, _, err := openStore(ctx, env)
	if err != nil {
		return false
	}
	claim, err := storeReducer(ctx, st)
	return err == nil && claim == env.settings.Host
}
