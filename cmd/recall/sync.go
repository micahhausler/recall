package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/archive"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/hook"
	"github.com/micahhausler/recall/internal/imports"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/store"
)

const syncUsage = `Usage:
  recall sync [--force]
        run each step that is due, in order: the configured imports and
        propose on the proposing machine only, then archive and observe on
        every machine. Imports run every sync; archive uploads each finished
        week the store lacks; observe reads sessions quiet for 6 hours and
        uploads its observations; propose runs a week after its last run,
        whatever hooks.learn_every says.
        --force runs every step whether or not it is due, and observes
        sessions with no quiet period; it still never imports or proposes
        off the proposing machine.
`

// proposeEvery is how long after a propose run the next is due.
const proposeEvery = 7 * 24 * time.Hour

type syncOpts struct {
	force bool
}

// syncStep is one step's result in the sync state file.
type syncStep struct {
	Step   string    `json:"step"`
	Result string    `json:"result"`
	Detail string    `json:"detail,omitempty"`
	Time   time.Time `json:"time"`
}

// syncState is what the last sync did, for `recall status`.
type syncState struct {
	Started time.Time  `json:"started"`
	Steps   []syncStep `json:"steps"`
}

// Step results.
const (
	resultOK      = "ok"
	resultNothing = "nothing due"
	resultNotDue  = "not due"
	resultSkipped = "skipped"
	resultFailed  = "failed"
	resultNotRun  = "not run"
)

func cmdSync(args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	var o syncOpts
	fs.BoolVar(&o.force, "force", false, "run every step whether or not it is due")
	fs.Usage = func() { fmt.Fprint(os.Stderr, syncUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "recall: sync takes no argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	env, err := loadArchiveEnv()
	if err != nil {
		return fail(err)
	}
	return runSync(context.Background(), env, o, execSource)
}

// syncRun collects each step's result.
type syncRun struct {
	env   *archiveEnv
	state syncState
}

func (s *syncRun) record(step, result, detail string) {
	s.state.Steps = append(s.state.Steps, syncStep{Step: step, Result: result, Detail: detail, Time: s.env.clk.Now()})
}

func (s *syncRun) failed() bool {
	for _, st := range s.state.Steps {
		if st.Result == resultFailed {
			return true
		}
	}
	return false
}

// runSync runs each due step under the hook worker's lock; a failed step
// is reported and the later steps still run.
func runSync(ctx context.Context, env *archiveEnv, o syncOpts,
	newSource func(name string, imp *config.ImportSettings) imports.Source) int {
	dir := hookStateDir(env)
	now := env.clk.Now()
	lock := filepath.Join(dir, "hook.lock")
	release, ok := hook.Lock(lock, workerStale, now)
	if !ok {
		fmt.Fprintf(os.Stderr, "recall: sync: another sync or the session hook's worker holds %s; nothing ran\n", tilde(lock, env.home))
		return 1
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, workerStale-10*time.Minute)
	defer cancel()
	s := &syncRun{env: env, state: syncState{Started: now}}

	claim, claimErr := storeClaim(ctx, env)
	s.imports(ctx, claim, claimErr, newSource)
	s.archive(ctx)
	runner, rerr := env.newRunner()
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "recall: observe: %v\n", rerr)
		s.record("observe", resultFailed, rerr.Error())
	} else {
		s.observe(ctx, runner, o.force)
	}
	s.upload(ctx)
	if rerr != nil {
		fmt.Printf("propose: not run; %v\n", rerr)
		s.record("propose", resultNotRun, rerr.Error())
	} else {
		res, detail := proposeStep(ctx, env, runner, o.force)
		s.record("propose", res, detail)
	}
	if err := saveSyncState(config.SyncStatePath(env.home), s.state); err != nil {
		fmt.Fprintf(os.Stderr, "recall: sync: %v\n", err)
	}
	if s.failed() {
		return 1
	}
	return 0
}

// claimSkip says why this machine doesn't import or propose, or "" when
// it holds the claim.
func claimSkip(env *archiveEnv, claim string, err error, what string) string {
	switch {
	case err != nil:
		return fmt.Sprintf("couldn't read which machine proposes for this store (%v)", firstLine(err))
	case claim == "":
		return "no machine proposes for this store yet; `recall init --claim` makes this one the proposing machine"
	case claim != env.settings.Host:
		return fmt.Sprintf("%s %s", claim, what)
	}
	return ""
}

func firstLine(err error) string { return strings.SplitN(err.Error(), "\n", 2)[0] }

func (s *syncRun) imports(ctx context.Context, claim string, claimErr error,
	newSource func(name string, imp *config.ImportSettings) imports.Source) {
	names := importNames(s.env)
	if len(names) == 0 {
		fmt.Println("import: none configured")
		return
	}
	why := claimSkip(s.env, claim, claimErr, "proposes for this store and runs its imports")
	for _, name := range names {
		step := "import " + name
		if why != "" {
			fmt.Printf("%s: skipped; %s\n", step, why)
			s.record(step, resultSkipped, why)
			continue
		}
		if err := runImport(ctx, s.env, name, importFlags{}, newSource); err != nil {
			fmt.Fprintf(os.Stderr, "recall: import %s: %v\n", name, err)
			s.record(step, resultFailed, firstLine(err))
			continue
		}
		s.record(step, resultOK, "")
	}
}

func (s *syncRun) archive(ctx context.Context) {
	st, dest, err := openStore(ctx, s.env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: archive: %v\n", err)
		s.record("archive", resultFailed, firstLine(err))
		return
	}
	r := newArchiveRunner(s.env)
	r.Store, r.Dest, r.Quiet = st, dest, true
	r.Out = prefixWriter{"archive: "}
	failed, err := r.Run(ctx, archive.DefaultLookback, false, false)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "recall: archive: %v\n", err)
		s.record("archive", resultFailed, firstLine(err))
	case failed > 0:
		s.record("archive", resultFailed, fmt.Sprintf("%d upload(s) failed", failed))
	case r.Uploaded == 0:
		fmt.Println("archive: nothing due; the store holds every finished week with sessions")
		s.record("archive", resultNothing, "")
	default:
		s.record("archive", resultOK, fmt.Sprintf("%d week(s) uploaded", r.Uploaded))
	}
}

// prefixWriter prints each line it gets with a prefix.
type prefixWriter struct{ prefix string }

func (p prefixWriter) Write(b []byte) (int, error) {
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		fmt.Println(p.prefix + l)
	}
	return len(b), nil
}

func (s *syncRun) observe(ctx context.Context, runner model.Runner, force bool) {
	idle := mapIdle
	if force {
		idle = 0
	}
	p, err := planMap(ctx, s.env, runner, fmt.Sprintf("%dd", mapWindowDays), 0, false, idle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: observe: %v\n", err)
		s.record("observe", resultFailed, firstLine(err))
		return
	}
	p.syncing = true
	code := p.execute(ctx, nil, nil)
	r := p.run
	detail := fmt.Sprintf("%d observed, %d cached", r.mapped, r.cached)
	switch {
	case code != 0:
		s.record("observe", resultFailed, detail)
	case r.mapped+r.judged == 0:
		s.record("observe", resultNothing, detail)
	default:
		s.record("observe", resultOK, detail)
	}
}

func (s *syncRun) upload(ctx context.Context) {
	if s.env.settings.Store.Kind == "fs" {
		return // observe wrote to the store
	}
	n, err := uploadObservations(ctx, s.env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: upload: %v\n", err)
		s.record("upload", resultFailed, firstLine(err))
		return
	}
	if n == 0 {
		fmt.Println("upload: nothing waits")
		s.record("upload", resultNothing, "")
		return
	}
	fmt.Printf("upload: %d %s to the store\n", n, plural(n, "object", "objects"))
	s.record("upload", resultOK, fmt.Sprintf("%d uploaded", n))
}

// uploadObservations copies this machine's waiting observations to the
// store; on an fs store observe wrote there directly.
func uploadObservations(ctx context.Context, env *archiveEnv) (int, error) {
	if env.settings.Store.Kind == "fs" {
		return 0, nil
	}
	st, _, err := openStore(ctx, env)
	if err != nil {
		return 0, err
	}
	p, err := store.NewPending(config.PendingDir(env.home), config.UploadedDir(env.home), st)
	if err != nil {
		return 0, err
	}
	return p.Upload(ctx, os.Stderr)
}

// proposeStep proposes on the proposing machine once a week, through the
// hook worker's two stamps: hook-learn.stamp marks a settled run, and
// hook-learn-tried.stamp spaces tries a day apart. force skips both, never
// the claim.
func proposeStep(ctx context.Context, env *archiveEnv, runner model.Runner, force bool) (result, detail string) {
	say := func(result, detail string) (string, string) {
		fmt.Printf("propose: %s; %s\n", result, detail)
		return result, detail
	}
	claim, err := storeClaim(ctx, env)
	if why := claimSkip(env, claim, err, "is the proposing machine for this store"); why != "" {
		return say(resultSkipped, why)
	}
	dir := hookStateDir(env)
	done, tried := filepath.Join(dir, "hook-learn.stamp"), filepath.Join(dir, "hook-learn-tried.stamp")
	now := env.clk.Now()
	if !force {
		if !hook.Due(done, proposeEvery, now) {
			return say(resultNotDue, "proposals last ran "+stampTime(done)+"; the next run is a week after")
		}
		if !hook.Due(tried, retryGap, now) {
			return say(resultNotDue, "the last try at "+stampTime(tried)+" didn't finish; the next is a day after it")
		}
	}
	if err := waitingGate(env); err != nil {
		return say(resultNotRun, err.Error())
	}
	_ = hook.Touch(tried, now)
	code, settled := learnRun(ctx, env, runner, "", reduceWindow{}, false)
	if code != 0 {
		fmt.Println("propose: failed; the next sync a day from now tries again")
		return resultFailed, fmt.Sprintf("exit %d", code)
	}
	if settled {
		_ = hook.Touch(done, now)
	}
	return resultOK, ""
}

func stampTime(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "never"
	}
	return st.ModTime().Format("2006-01-02 15:04")
}

func saveSyncState(path string, s syncState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadSyncState reads the last sync's results; ok is false before any.
func loadSyncState(path string) (s syncState, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &s) != nil {
		return syncState{}, false
	}
	return s, true
}
