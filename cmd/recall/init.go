package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/review"
	"github.com/micahhausler/recall/internal/store"
)

const initUsage = `Usage:
  recall init [--dry-run]

Sets a new machine up with defaults that need nothing else: the store is a
local directory, proposals are reviewed on a local branch, and the plugin's
session hooks run the loop, so there is no bucket, no cron job or
LaunchAgent, no git remote, and no TOML to write by hand.

The session hooks are the default way to run the loop; recur jobs are the
opt-in for a fixed schedule. It only sets keys no config file sets yet,
through the same drop-in ` + "`recall config set`" + ` writes, and it leaves a hook
step off when a recur job's command already runs that step, so nothing runs
twice. The weekly reduce turns on only when no other machine has claimed it
on the shared store. On a machine that already has config, review stays on
pull requests, the default; init prints the command that moves it to a
local branch. It calls no model. Safe to run again.
`

func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print what would be set; change nothing")
	fs.Usage = func() { fmt.Fprint(os.Stderr, initUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	base := config.DefaultBase(home)
	files, err := config.Files(base)
	if err != nil {
		return fail(err)
	}
	c, err := config.Load(base)
	if err != nil {
		return fail(err)
	}
	env, err := loadArchiveEnv()
	if err != nil {
		return fail(err)
	}
	ctx := context.Background()
	// One machine per store runs the reduce: the first init on a store
	// claims it under learn/reducer, and a later init on another machine
	// sees the claim and leaves hooks.learn_every off. Asked only when the
	// key would otherwise turn on.
	reducer := func() (string, error) {
		st, err := initStore(ctx, env, *dryRun)
		if st == nil || err != nil {
			return "", err
		}
		host, err := storeReducer(ctx, st)
		if err != nil || host == env.settings.Host {
			return "", err // this machine's own claim doesn't hold it back
		}
		return host, nil
	}
	plan, notes := config.Fresh(c, len(files) > 0, env.settings.Host, reducer)
	for _, kv := range plan {
		verb := "set"
		if *dryRun {
			verb = "would set"
		}
		fmt.Printf("%s %s = %s\n    %s\n", verb, kv.Key, kv.Value, kv.Why)
		if *dryRun {
			continue
		}
		if _, err := config.Set(base, kv.Key, kv.Value); err != nil {
			return fail(err)
		}
	}
	for _, note := range notes {
		fmt.Printf("keep %s\n", note)
	}
	if len(plan) == 0 {
		fmt.Println("config: nothing to set; every key init would choose is already set")
	}
	if *dryRun {
		return 0
	}

	// Reload: the plan above may have set keys.
	env, err = loadArchiveEnv()
	if err != nil {
		return fail(err)
	}
	s := env.settings
	if s.Store.Kind == "fs" {
		if _, _, err := openStore(ctx, env); err != nil {
			return fail(err)
		}
		fmt.Printf("store: %s\n", tilde(s.Store.Path, env.home))
	} else {
		fmt.Printf("store: %s, as configured; the archive-chats skill sets a bucket up\n", s.Store.Kind)
	}
	// This machine runs the reduce, so claim it for the store; the first
	// claim wins, and a re-run on the same host changes nothing.
	if s.Hooks.LearnEvery > 0 && !s.Recur.Covers(config.StepLearn) {
		if st, _, err := openStore(ctx, env); err != nil {
			fmt.Printf("warning: couldn't record this host as the store's reducer: %v\n", err)
		} else if err := claimReducer(ctx, st, s.Host); err != nil {
			fmt.Printf("warning: couldn't record this host as the store's reducer: %v\n", err)
		}
	}
	if _, err := exec.LookPath(s.Learn.Runner); err != nil {
		fmt.Printf("warning: %s isn't on PATH; the map, reduce, and apply run through it\n", s.Learn.Runner)
	}
	if exe, err := os.Executable(); err == nil {
		if msg := offGUIPath(exe, guiPaths(runtime.GOOS)); msg != "" {
			fmt.Println(msg)
		}
	}
	// Review stages edits in a git worktree, so every target has to sit in
	// a checkout. Say which don't, and how to fix it; change nothing.
	for _, target := range s.Learn.Targets {
		if _, ok := review.RepoRoot(ctx, target); !ok {
			fmt.Printf("warning: %s isn't in a git checkout, and review needs one; create it with: git init %s\n",
				tilde(target, env.home), tilde(filepath.Dir(target), env.home))
		}
	}
	if code := learnRulesSync(ctx, env); code != 0 {
		return code
	}
	hooks := s.Hooks.Map || s.Hooks.Archive || s.Hooks.LearnEvery > 0
	fmt.Println("\nNext:")
	if hooks {
		fmt.Println("  1. Install the plugin, which carries the session hook and the skills:")
		fmt.Println("       claude plugin marketplace add micahhausler/recall && claude plugin install recall@recall")
	} else {
		fmt.Println("  1. Your scheduled jobs run the loop, so the session hook has nothing to do (recall recur list).")
	}
	fmt.Println("  2. See what a first pass over your sessions would cost, then run it:")
	fmt.Printf("       recall learn map --since %dd --dry-run\n", mapWindowDays)
	fmt.Printf("       recall learn map --since %dd && recall learn run\n", mapWindowDays)
	fmt.Printf("  3. Review what it proposes: learn.review is %q (recall learn reviews).\n", s.Learn.Review)
	return 0
}

// reducerKey is the store object that records the one host that runs the
// reduce on that store, so two machines sharing it don't both propose.
const reducerKey = "learn/reducer"

// initStore opens the configured store for init's bookkeeping. In a dry run
// a missing fs store is reported as absent rather than created, so
// --dry-run changes nothing on disk.
func initStore(ctx context.Context, env *archiveEnv, dryRun bool) (store.Store, error) {
	if dryRun && env.settings.Store.Kind == "fs" {
		if _, err := os.Stat(env.settings.Store.Path); os.IsNotExist(err) {
			return nil, nil
		}
	}
	st, _, err := openStore(ctx, env)
	return st, err
}

// storeReducer reads the host that claimed the reduce on this store, or ""
// when none has.
func storeReducer(ctx context.Context, st store.Store) (string, error) {
	r, err := st.Get(ctx, reducerKey)
	if errors.Is(err, store.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, 1024))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// claimReducer records host as the store's reducer. The store writes only
// absent keys, so the first claim wins and a later one changes nothing.
func claimReducer(ctx context.Context, st store.Store, host string) error {
	err := st.PutIfAbsent(ctx, reducerKey, strings.NewReader(host+"\n"))
	if errors.Is(err, store.ErrExists) {
		return nil
	}
	return err
}

// guiPaths is the PATH a GUI-launched harness gets, which reads no shell
// profile: the system defaults, plus Homebrew's directories on macOS.
func guiPaths(goos string) []string {
	paths := []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	if goos == "darwin" {
		paths = append(paths, "/opt/homebrew/bin", "/usr/local/bin")
	}
	return paths
}

// offGUIPath says when the recall binary at exe isn't reachable from the
// PATH a GUI-launched harness gets, so the session hooks there would never
// find it; empty when it is. A link on one of those paths counts, which is
// how a Homebrew install lands.
func offGUIPath(exe string, paths []string) string {
	dir := filepath.Dir(exe)
	for _, p := range paths {
		if dir == p {
			return ""
		}
		if st, err := os.Stat(filepath.Join(p, filepath.Base(exe))); err == nil && st.Mode().IsRegular() {
			return ""
		}
	}
	return fmt.Sprintf("warning: %s isn't on the PATH a GUI-launched harness gets (%s), so its session hooks won't find recall; "+
		"link the binary into one of those directories", dir, strings.Join(paths, ":"))
}
