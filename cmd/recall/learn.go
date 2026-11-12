package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/micahhausler/recall/internal/archive"
	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/extension"
	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/learn"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/review"
	"github.com/micahhausler/recall/internal/store"
)

const learnUsage = `Usage:
  recall learn map [--since 30d|all] [--limit N] [--archives] [--dry-run]
        observe new or changed sessions with the map model; cached in the store.
        --archives also maps other hosts' archived weeks; by default each
        host maps its own sessions and the reduce reads every host's
  recall learn observations [--harness claude]
        print the cached observations as JSONL
  recall learn rules sync
        reconcile the rule ledger with the bullets in learn.targets
  recall learn age
        report rules the ledger says to reword or retire; exit 1 if any
  recall learn reduce [--out DIR] [--dry-run]
        fold the window's observations and the ledger into proposals for review
  recall learn apply [--from DIR] [--dry-run]
        stage a reduce run's proposals (default: the newest) as edits in fresh
        worktrees, one per repo they touch, and send each changed repo to
        review (learn.review: a pull request, or a local branch)
  recall learn run [--out DIR]
        rules sync, reduce, then apply: the scheduled form of the loop
  recall learn reviews
        check what became of applied proposals (merged, closed, pending) and
        list them; the reduce and apply do the same check first

The map, reduce, and apply steps call a model through the harness CLI
(learn.runner, default claude -p); --dry-run builds and prints what would be
sent instead. Nothing is written to a live file: apply edits worktrees, and
every change waits for your review.
`

func cmdLearn(args []string) int {
	fs := flag.NewFlagSet("learn", flag.ContinueOnError)
	since := fs.String("since", "30d", "map: only sessions with events in this window")
	limit := fs.Int("limit", 0, "map: at most this many sessions, newest first")
	fromArchives := fs.Bool("archives", false, "map: also map other hosts' archived sessions from the store (downloads their weeks)")
	outDir := fs.String("out", "", "reduce, run: where proposals land (default ~/.local/state/recall/learn/<date>)")
	from := fs.String("from", "", "apply: the reduce run to apply (default the newest under ~/.local/state/recall/learn)")
	dryRun := fs.Bool("dry-run", false, "build model inputs and report; call no model")
	harnessName := fs.String("harness", "claude", "observations: which harness")
	fs.Usage = func() { fmt.Fprint(os.Stderr, learnUsage) }
	cmd := ""
	rest := args
	if len(args) > 0 && args[0][0] != '-' {
		cmd = args[0]
		rest = args[1:]
	}
	if cmd == "rules" && len(rest) > 0 && rest[0] == "sync" {
		cmd, rest = "rules-sync", rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	env, err := loadArchiveEnv()
	if err != nil {
		return fail(err)
	}
	ctx := context.Background()
	runner := &model.CLI{Command: env.settings.Learn.Runner}

	switch cmd {
	case "map":
		return learnMap(ctx, env, runner, *since, *limit, *dryRun, *fromArchives)
	case "observations":
		return learnObservations(ctx, env, *harnessName)
	case "rules-sync":
		return learnRulesSync(env)
	case "age":
		return learnAge(env)
	case "reduce":
		return learnReduce(ctx, env, runner, *outDir, *dryRun)
	case "apply":
		return learnApply(ctx, env, *from, *dryRun)
	case "reviews":
		return learnReviews(ctx, env)
	case "run":
		if *dryRun {
			fmt.Fprintln(os.Stderr, "recall: learn run has no --dry-run; use learn reduce --dry-run, then learn apply --dry-run")
			return 2
		}
		return learnRun(ctx, env, runner, *outDir)
	default:
		fmt.Fprint(os.Stderr, learnUsage)
		return 2
	}
}

func loadLedgerAndHits(ctx context.Context, env *archiveEnv, st store.Store) (*learn.Ledger, error) {
	ledger, err := learn.LoadLedger(env.settings.Learn.Ledger)
	if err != nil {
		return nil, err
	}
	for _, h := range env.harnesses {
		window, err := learn.ListObservations(ctx, st, h.Name())
		if err != nil {
			return nil, err
		}
		ledger.ApplyHits(window)
	}
	return ledger, nil
}

// mapJob is one session waiting for the map step.
type mapJob struct {
	m        *learn.Mapper
	harness  string
	session  string
	evs      []events.Event
	label    string
	readFrom string // the archive key, for a session read from the store
}

// mapRun is one learn map pass: the counts it reports and the failure
// streak that stops it. Workers share it under mu.
type mapRun struct {
	mu                                               sync.Mutex
	mapped, cached, skipped, failed, partial, streak int
	cancel                                           context.CancelFunc
}

// inWindow reports whether any of a session's events fall at or after
// cutoff (an RFC 3339 UTC string).
func inWindow(evs []events.Event, cutoff string) bool {
	for _, e := range evs {
		if e.Timestamp >= cutoff {
			return true
		}
	}
	return false
}

func newJob(m *learn.Mapper, harnessName, session string, evs []events.Event) mapJob {
	return mapJob{m: m, harness: harnessName, session: session, evs: evs,
		label: harnessName + "/" + session + " @" + m.Host}
}

// dryRun reports what mapping the job would do, without a model call.
func (r *mapRun) dryRun(ctx context.Context, j mapJob) {
	call, err := j.m.WouldCall(ctx, j.harness, j.session, j.evs)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "recall: %s: %v (skipped)\n", j.label, err)
		r.skipped++
	case call:
		fmt.Printf("would map %s (%d events)\n", j.label, len(j.evs))
		r.mapped++
	default:
		r.cached++
	}
}

// work maps one job and records the result.
func (r *mapRun) work(ctx context.Context, j mapJob) {
	so, ran, err := j.m.MapSession(ctx, j.harness, j.session, j.evs)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			return // stopped on purpose; not this session's failure
		}
		// One session the model can't read doesn't stop the rest; it isn't
		// cached, so the next run retries it. A run of failures in a row
		// looks like auth or an outage, so stop rather than spend on it.
		fmt.Fprintf(os.Stderr, "recall: %v (failed)\n", err) // names the session
		r.failed++
		r.streak++
		if r.streak == maxFailStreak {
			fmt.Fprintf(os.Stderr, "recall: %d sessions failed in a row; stopping\n", r.streak)
			r.cancel()
		}
		return
	}
	r.streak = 0
	if !ran {
		r.cached++
		return
	}
	r.mapped++
	note := ""
	if p := so.Partial; p != nil {
		note = fmt.Sprintf("; %s: read the first %d of %d prompts", p.Reason, p.Prompts, p.Of)
		r.partial++
	}
	fmt.Printf("observed %s: %d observation(s), %d rule hit(s) ($%.4f%s)\n",
		j.label, len(so.Observations), len(so.RuleHits), so.CostUSD, note)
}

// runAll maps the jobs with workers running at once, in order, until done
// or stopped by a failure streak.
func (r *mapRun) runAll(ctx context.Context, jobs []mapJob, workers int) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.cancel = cancel
	ch := make(chan mapJob)
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				r.work(ctx, j)
			}
		}()
	}
feed:
	for _, j := range jobs {
		select {
		case ch <- j:
		case <-ctx.Done():
			break feed
		}
	}
	close(ch)
	wg.Wait()
}

func learnMap(ctx context.Context, env *archiveEnv, runner model.Runner, since string, limit int, dryRun, fromArchives bool) int {
	cutoffTime, err := parseSince(since, env.clk.Now())
	if err != nil {
		return fail(err)
	}
	cutoff := cutoffTime.UTC().Format("2006-01-02T15:04:05Z")
	st, _, err := openStore(ctx, env)
	if err != nil {
		return fail(err)
	}
	ledger, err := learn.LoadLedger(env.settings.Learn.Ledger)
	if err != nil {
		return fail(err)
	}
	run := &mapRun{}
	var mappers []*learn.Mapper
	mapper := func(host string) *learn.Mapper {
		m := &learn.Mapper{
			Store: st, Runner: runner, Model: env.settings.Learn.MapModel,
			Host: host, Rules: ledger.Rules, Clock: env.clk, Out: os.Stdout,
		}
		mappers = append(mappers, m)
		return m
	}
	var jobs []mapJob
	for _, h := range env.harnesses {
		m := mapper(env.settings.Host)
		sessions, err := h.Sessions()
		if err != nil {
			return fail(err)
		}
		// Newest first, so --limit spends the budget on recent sessions.
		mtimes := map[string]int64{}
		for _, s := range sessions {
			if st, err := os.Stat(s); err == nil {
				mtimes[s] = st.ModTime().UnixNano()
			}
		}
		sort.Slice(sessions, func(i, j int) bool { return mtimes[sessions[i]] > mtimes[sessions[j]] })
		for _, s := range sessions {
			evs, err := h.ReadSessionFile(s, env.settings.Host)
			if err != nil {
				fmt.Fprintf(os.Stderr, "recall: %s: %v (skipped)\n", s, err)
				run.skipped++
				continue
			}
			if inWindow(evs, cutoff) {
				session := filepath.Base(s)
				jobs = append(jobs, newJob(m, h.Name(), session[:len(session)-len(filepath.Ext(session))], evs))
			}
		}
		if fromArchives {
			archived, err := archiveJobs(ctx, env, st, h, cutoffTime, cutoff, mapper)
			if err != nil {
				return fail(err)
			}
			jobs = append(jobs, archived...)
		}
	}
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	if dryRun {
		for _, j := range jobs {
			run.dryRun(ctx, j)
		}
		fmt.Printf("map --dry-run: %d session(s) would call the model; %d are cached or have no prompts\n", run.mapped, run.cached)
		return 0
	}
	run.runAll(ctx, jobs, env.settings.Learn.MapWorkers)
	var usage model.Usage
	for _, m := range mappers {
		usage = usage.Add(m.Usage)
	}
	fmt.Printf("map: %d observed, %d cached", run.mapped, run.cached)
	if run.skipped > 0 {
		fmt.Printf(", %d skipped", run.skipped)
	}
	if run.partial > 0 {
		fmt.Printf(", %d partly read after a refusal", run.partial)
	}
	if run.failed > 0 {
		fmt.Printf(", %d failed", run.failed)
	}
	if usage.CostUSD > 0 || usage.InputTokens > 0 {
		fmt.Printf(" (%s)", usage)
	}
	fmt.Println()
	if !fromArchives {
		warnUnobservedHosts(ctx, env, st)
	}
	if run.failed > 0 {
		return 1
	}
	return 0
}

// archiveJobs reads other hosts' archived sessions from the store, for
// weeks that overlap the window. A session archived in more than one week
// is read from its latest week, which holds the whole file.
func archiveJobs(ctx context.Context, env *archiveEnv, st store.Store, h harness.Harness, cutoffTime time.Time,
	cutoff string, mapper func(host string) *learn.Mapper) ([]mapJob, error) {
	keys, err := st.List(ctx, h.Name()+"/")
	if err != nil {
		return nil, err
	}
	byHost := map[string][]string{}
	var hosts []string
	for _, key := range keys {
		_, host, w, ok := archive.ParseKey(key)
		if !ok || host == env.settings.Host {
			continue
		}
		if _, end := w.Bounds(time.UTC); end.Before(cutoffTime) {
			continue
		}
		if byHost[host] == nil {
			hosts = append(hosts, host)
		}
		byHost[host] = append(byHost[host], key) // List is sorted: oldest week first
	}
	var jobs []mapJob
	for _, host := range hosts {
		sessions := map[string][]events.Event{}
		var order []string
		for _, key := range byHost[host] {
			fmt.Fprintf(os.Stderr, "recall: reading %s\n", key)
			r, err := st.Get(ctx, key)
			if err != nil {
				return nil, err
			}
			evs, err := harness.ReadTarZstFrom(h, r, host)
			r.Close()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			week := map[string][]events.Event{}
			for _, e := range evs {
				week[e.Session] = append(week[e.Session], e)
			}
			for s, se := range week {
				if _, seen := sessions[s]; !seen {
					order = append(order, s)
				}
				sessions[s] = se
			}
		}
		m := mapper(host)
		for _, s := range order {
			if inWindow(sessions[s], cutoff) {
				jobs = append(jobs, newJob(m, h.Name(), s, sessions[s]))
			}
		}
	}
	return jobs, nil
}

// unobservedDays is how long a host may archive sessions with no
// observations from it before learn warns.
const unobservedDays = 14

// warnUnobservedHosts names hosts whose recent archives have no recent
// observations: their sessions reach the archive but never the reduce.
func warnUnobservedHosts(ctx context.Context, env *archiveEnv, st store.Store) {
	since := env.clk.Now().AddDate(0, 0, -unobservedDays)
	for _, h := range env.harnesses {
		keys, err := st.List(ctx, h.Name()+"/")
		if err != nil {
			return
		}
		archived := map[string]bool{}
		for _, key := range keys {
			if _, host, w, ok := archive.ParseKey(key); ok {
				if _, end := w.Bounds(time.UTC); !end.Before(since) {
					archived[host] = true
				}
			}
		}
		obs, err := learn.ListObservations(ctx, st, h.Name())
		if err != nil {
			return
		}
		for _, so := range obs {
			if t, err := time.Parse(time.RFC3339, so.ObservedAt); err == nil && !t.Before(since) {
				delete(archived, so.Host)
			}
		}
		hosts := make([]string, 0, len(archived))
		for host := range archived {
			hosts = append(hosts, host)
		}
		sort.Strings(hosts)
		for _, host := range hosts {
			fmt.Fprintf(os.Stderr, "recall: warning: %s archived %s sessions in the last %d days but has no observations; "+
				"run `recall learn map` there, or `recall learn map --archives` here\n", host, h.Name(), unobservedDays)
		}
	}
}

// maxFailStreak is how many sessions in a row may fail to map before the
// run stops.
const maxFailStreak = 3

func learnObservations(ctx context.Context, env *archiveEnv, harnessName string) int {
	st, _, err := openStore(ctx, env)
	if err != nil {
		return fail(err)
	}
	window, err := learn.ListObservations(ctx, st, harnessName)
	if err != nil {
		return fail(err)
	}
	for _, so := range window {
		if err := printJSON(so); err != nil {
			return fail(err)
		}
	}
	return 0
}

func learnRulesSync(env *archiveEnv) int {
	ledger, err := learn.LoadLedger(env.settings.Learn.Ledger)
	if err != nil {
		return fail(err)
	}
	added, removed, err := ledger.SyncTargets(env.settings.Learn.Targets, env.clk.Now())
	if err != nil {
		return fail(err)
	}
	if err := ledger.Save(env.settings.Learn.Ledger); err != nil {
		return fail(err)
	}
	fmt.Printf("ledger %s: %d rule(s), %d added, %d removed\n",
		tilde(env.settings.Learn.Ledger, env.home), len(ledger.Rules), added, removed)
	return 0
}

func learnAge(env *archiveEnv) int {
	ctx := context.Background()
	st, _, err := openStore(ctx, env)
	if err != nil {
		return fail(err)
	}
	ledger, err := loadLedgerAndHits(ctx, env, st)
	if err != nil {
		return fail(err)
	}
	aging := ledger.Age(env.clk.Now(), env.settings.Learn.RetireDays)
	for _, r := range aging.Reword {
		fmt.Printf("reword %s (last violated %s): %s\n", r.ID, r.LastViolated, r.Text)
	}
	for _, r := range aging.Retire {
		fmt.Printf("retire %s (no relevance in %d days): %s\n", r.ID, env.settings.Learn.RetireDays, r.Text)
	}
	if len(aging.Reword)+len(aging.Retire) > 0 {
		return 1
	}
	fmt.Println("recall learn age: every rule has earned its place")
	return 0
}

// learnStateRoot is where reduce runs land, one dated directory each.
func learnStateRoot(env *archiveEnv) string {
	return filepath.Join(env.home, ".local", "state", "recall", "learn")
}

func learnReduce(ctx context.Context, env *archiveEnv, runner model.Runner, outDir string, dryRun bool) int {
	if _, err := reduce(ctx, env, runner, outDir, dryRun); err != nil {
		return fail(err)
	}
	return 0
}

// reduce runs the reduce into outDir and returns that directory; a dry run
// prints the input and returns "".
func reduce(ctx context.Context, env *archiveEnv, runner model.Runner, outDir string, dryRun bool) (string, error) {
	st, _, err := openStore(ctx, env)
	if err != nil {
		return "", err
	}
	ledger, err := loadLedgerAndHits(ctx, env, st)
	if err != nil {
		return "", err
	}
	refreshReviews(ctx, env, ledger)
	var window []learn.SessionObservations
	for _, h := range env.harnesses {
		w, err := learn.ListObservations(ctx, st, h.Name())
		if err != nil {
			return "", err
		}
		window = append(window, w...)
	}
	taken, err := takenList(ctx, env)
	if err != nil {
		return "", err
	}
	r := &learn.Reducer{
		Runner: runner, Model: env.settings.Learn.ReduceModel, Ledger: ledger,
		RetireDays: env.settings.Learn.RetireDays, Clock: env.clk, Out: os.Stdout,
		Taken: taken, Targets: env.settings.Learn.Targets, Home: env.home,
		Skills: env.settings.Learn.Skills, Bin: env.settings.Learn.Bin, Reviews: ledger.Reviews,
		OwnerInstructions: extension.Read(env.settings.Extensions["learn-reduce"], os.Stderr),
	}
	warnUnobservedHosts(ctx, env, st)
	if dryRun {
		fmt.Print(r.BuildInput(window))
		return "", nil
	}
	if outDir == "" {
		outDir = filepath.Join(learnStateRoot(env), env.clk.Now().Format("2006-01-02"))
	}
	proposals, usage, err := r.Run(ctx, window, outDir)
	if err != nil {
		return "", err
	}
	// The ledger's relevance dates moved with the window's rule hits.
	if err := ledger.Save(env.settings.Learn.Ledger); err != nil {
		return "", err
	}
	fmt.Printf("proposals: %s (input beside it) (%s)\n", tilde(proposals, env.home), usage)
	return outDir, nil
}

// takenList is every skill and command name that already exists.
func takenList(ctx context.Context, env *archiveEnv) ([]string, error) {
	evs, err := readEvents(env, time.Time{})
	if err != nil {
		return nil, err
	}
	n, err := takenNames(ctx, env, nil, evs)
	if err != nil {
		return nil, err
	}
	return n.taken(), nil
}

func newApplier(ctx context.Context, env *archiveEnv) (*learn.Applier, error) {
	pub, err := review.New(env.settings.Learn.Review)
	if err != nil {
		return nil, fmt.Errorf("learn.review: %w", err)
	}
	taken, err := takenList(ctx, env)
	if err != nil {
		return nil, err
	}
	l := env.settings.Learn
	return &learn.Applier{
		Editor: &model.CLI{Command: l.Runner}, Model: l.ApplyModel, Publisher: pub,
		Targets: l.Targets, Skills: l.Skills, Bin: l.Bin, Public: env.settings.Keep.Public,
		Taken: taken, Home: env.home, Check: l.Check, Clock: env.clk, Out: os.Stderr,
		OwnerInstructions: extension.Read(env.settings.Extensions["learn-apply"], os.Stderr),
	}, nil
}

func learnApply(ctx context.Context, env *archiveEnv, dir string, dryRun bool) int {
	if dir == "" {
		latest, err := learn.LatestRun(learnStateRoot(env))
		if err != nil {
			return fail(err)
		}
		dir = latest
	}
	return applyRun(ctx, env, dir, dryRun)
}

func applyRun(ctx context.Context, env *archiveEnv, dir string, dryRun bool) int {
	a, err := newApplier(ctx, env)
	if err != nil {
		return fail(err)
	}
	ledger, err := learn.LoadLedger(env.settings.Learn.Ledger)
	if err != nil {
		return fail(err)
	}
	refreshReviews(ctx, env, ledger)
	a.Reviews = ledger.Reviews
	plan, err := a.Plan(ctx, dir)
	if errors.Is(err, learn.ErrApplied) {
		fmt.Printf("apply: %s is already applied\n", tilde(dir, env.home))
		return 0
	}
	if err != nil {
		return fail(err)
	}
	for _, r := range plan.Routes {
		if r.Skip != "" {
			fmt.Printf("skip %s: %s: %s\n", r.Kind, r.Title, r.Skip)
		} else {
			fmt.Printf("apply %s: %s\n", r.Kind, r.Title)
		}
	}
	if dryRun {
		for i, repo := range plan.Repos {
			fmt.Printf("would stage %s on branch %s at %s\n", tilde(repo, env.home), plan.Branch, tilde(plan.Worktrees[i], env.home))
		}
		fmt.Printf("\n%s", a.BuildInput(plan, learn.PlannedWorktrees(plan)))
		return 0
	}
	if plan.Ready() == 0 {
		fmt.Println("apply: nothing to apply")
	}
	res, err := a.Run(ctx, plan)
	if res != nil {
		// What went to review is pending until the owner decides; the next
		// reduce and apply read it from the ledger.
		ledger.RecordReviews(plan, res, env.settings.Learn.Review)
		if serr := ledger.Save(env.settings.Learn.Ledger); serr != nil {
			fmt.Fprintf(os.Stderr, "recall: saving the review ledger: %v\n", serr)
		}
		held := map[string]bool{} // skips the plan already printed
		for _, r := range plan.Routes {
			if r.Skip != "" {
				held[r.Kind+":"+r.Title] = true
			}
		}
		for _, o := range res.Outcomes {
			if !o.Applied && !held[o.Kind+":"+o.Title] {
				fmt.Printf("not applied %s: %s: %s\n", o.Kind, o.Title, o.Detail)
			}
		}
		for _, l := range res.Changes {
			if l.Error != "" {
				fmt.Fprintf(os.Stderr, "recall: %s: %s\n", tilde(l.Repo, env.home), l.Error)
				continue
			}
			fmt.Printf("%s: %s\n", tilde(l.Repo, env.home), l.Where)
		}
		if plan.Ready() > 0 {
			fmt.Printf("apply: %d change(s) for review (%s)\n", len(res.Changes), res.Usage)
		}
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

// refreshReviews updates the ledger's pending reviews from their
// publishers. A publisher that can't answer is a warning, not a stop: its
// reviews stay pending and hold their proposals back.
func refreshReviews(ctx context.Context, env *archiveEnv, ledger *learn.Ledger) {
	n, err := ledger.RefreshReviews(ctx, review.New, env.clk.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: warning: checking reviews: %v\n", err)
	}
	if n > 0 {
		fmt.Printf("reviews: %d decided since the last check\n", n)
	}
}

func learnReviews(ctx context.Context, env *archiveEnv) int {
	ledger, err := learn.LoadLedger(env.settings.Learn.Ledger)
	if err != nil {
		return fail(err)
	}
	refreshReviews(ctx, env, ledger)
	if err := ledger.Save(env.settings.Learn.Ledger); err != nil {
		return fail(err)
	}
	if len(ledger.Reviews) == 0 {
		fmt.Println("reviews: none yet; recall learn apply records what it sends for review")
		return 0
	}
	for _, rv := range ledger.Reviews {
		when := "since " + rv.Proposed
		if rv.Verdict != "pending" {
			when = rv.Decided
		}
		rv.Repo = tilde(rv.Repo, env.home)
		fmt.Printf("%-7s %s  %s: %s  (%s)\n", rv.Verdict, when, rv.Kind, rv.Title, rv.Place())
	}
	return 0
}

// learnRun is the scheduled loop on one host: put new or hand-edited rules
// on the ledger, reduce, and apply what the reduce proposed.
func learnRun(ctx context.Context, env *archiveEnv, runner model.Runner, outDir string) int {
	if code := learnRulesSync(env); code != 0 {
		return code
	}
	dir, err := reduce(ctx, env, runner, outDir, false)
	if err != nil {
		return fail(err)
	}
	md, err := os.ReadFile(filepath.Join(dir, "proposals.md"))
	if err != nil {
		return fail(err)
	}
	if len(learn.ParseProposals(string(md))) == 0 {
		fmt.Println("run: no proposals this time")
		return 0
	}
	return applyRun(ctx, env, dir, false)
}

func printJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
