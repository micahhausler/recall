package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/micahhausler/recall/internal/archive"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/evidence"
	"github.com/micahhausler/recall/internal/extension"
	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/learn"
	"github.com/micahhausler/recall/internal/lint"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/review"
	"github.com/micahhausler/recall/internal/store"
)

const learnUsage = `Usage:
  recall learn map [--since 30d|all] [--limit N] [--idle 6h] [--archives] [--dry-run]
        observe new or changed sessions with learn.map_model; cached in the store.
        --idle skips a session active more recently than that, so one still
        going isn't observed now and again when it grows.
        --archives also observes archived weeks from the store: other hosts'
        sessions, and this host's own that are no longer on disk; a session
        still on disk is read from disk. By default each host observes its
        own sessions and the proposing machine reads every host's
  recall learn observations [--harness NAME]
        print the cached observations as JSONL
  recall learn candidates
        list the lessons the window's sessions showed, as propose will
        read them: alike observations grouped and counted, no model call
  recall learn report [--since 30d|all]
        say what recall noticed: the lessons the window's sessions repeat,
        with session counts, dates, and your own words, then a report card
        of your rules: broken, followed, or never relevant, with counts;
        no model call
  recall learn rules sync
        reconcile the rule ledger with the bullets in learn.targets and in the
        context files of each learn.repos checkout
  recall learn targets
        list every context file and note a proposal may land in, and its scope
  recall learn age
        report rules the ledger says to reword or retire; exit 1 if any.
        Also lists, as "check", rules earned on a model no recent session
        runs, which the current model may follow unprompted
  recall learn reduce [--out DIR] [--window 365d|all] [--dry-run] [--force]
        fold the window's observations and the ledger into proposals for
        review. The window defaults to learn.retire_days; --window reads
        that many days back, or every observation with all, without
        changing retirement, which always uses learn.retire_days.
        On a machine that isn't the store's proposing machine it refuses,
        since only that machine assigns rule ids; --force runs it anyway,
        saving locally and leaving the store ledger unwritten
  recall learn apply [--from DIR] [--dry-run]
        stage a run's proposals (default: the newest) as edits in fresh
        worktrees, one per repo they touch, and send each changed repo to
        review (learn.review: a pull request or a local branch), one commit
        per proposal so a reviewer can drop one before merging
  recall learn run [--out DIR] [--window 365d|all] [--force]
        rules sync, fold into proposals, then apply: the same step as
        recall propose
  recall learn reducer [--take]
        say which host is this store's proposing machine; --take moves the
        claim to this host, for when that machine is retired, reimaged,
        or renamed
  recall learn reviews
        check what became of applied proposals (merged, closed, pending) and
        list them; propose and apply do the same check first

Observing, proposing, and applying call a model through the harness CLI
(learn.runner, claude or kiro); --dry-run builds and prints what would be
sent instead. Nothing is written to a live file: apply edits worktrees, and
every change waits for your review.
`

// mapWindowDays is the default map window: `recall observe --since`
// covers this many days unless told otherwise, `recall init` prints it,
// and the hook worker's first run maps back over it.
const mapWindowDays = 30

// learnOpts are the learn steps' flags.
type learnOpts struct {
	since    string
	limit    int
	archives bool
	idle     time.Duration
	out      string
	window   string
	from     string
	dryRun   bool
	force    bool
	take     bool
	harness  string
}

// learnFlags registers the flags the named learn verbs use.
func learnFlags(name, usage string, verbs ...string) (*flag.FlagSet, *learnOpts) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	o := &learnOpts{since: fmt.Sprintf("%dd", mapWindowDays)}
	uses := func(want ...string) bool {
		for _, v := range verbs {
			for _, w := range want {
				if v == w {
					return true
				}
			}
		}
		return false
	}
	if uses("map", "report") {
		fs.StringVar(&o.since, "since", o.since, "observe: only sessions with events in this window; report: the window it reports on")
	}
	if uses("map") {
		fs.IntVar(&o.limit, "limit", 0, "observe: at most this many sessions, newest first")
		fs.BoolVar(&o.archives, "archives", false, "observe: also observe other hosts' archived sessions from the store (downloads their weeks)")
		fs.DurationVar(&o.idle, "idle", 0, "observe: skip a session whose transcript changed more recently than this")
	}
	if uses("reduce", "run") {
		fs.StringVar(&o.out, "out", "", "reduce, propose: where proposals land (default ~/.local/state/recall/learn/<date>)")
	}
	if uses("reduce", "run") {
		fs.StringVar(&o.window, "window", "", "reduce, propose: days of observations to read, like 365d, or \"all\"; retirement still uses learn.retire_days")
	}
	if uses("apply") {
		fs.StringVar(&o.from, "from", "", "apply: the reduce run to apply (default the newest under ~/.local/state/recall/learn)")
	}
	if uses("map", "reduce", "apply", "run") {
		fs.BoolVar(&o.dryRun, "dry-run", false, "build model inputs and report; call no model")
	}
	if uses("reduce", "run") {
		fs.BoolVar(&o.force, "force", false, "reduce, propose: run on a machine that isn't the store's proposing machine; the store ledger stays unwritten")
	}
	if uses("reducer") {
		fs.BoolVar(&o.take, "take", false, "reducer: move the store's claim to this host")
	}
	if uses("observations") {
		fs.StringVar(&o.harness, "harness", "", "observations: which harness (default the primary one: claude, or kiro with claude off)")
	}
	return fs, o
}

var learnVerbs = []string{"map", "observations", "candidates", "report", "rules-sync", "targets", "age", "reduce", "apply", "reviews", "reducer", "run"}

func cmdLearn(args []string) int {
	fs, o := learnFlags("learn", learnUsage, learnVerbs...)
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
	if cmd == "run" && o.dryRun {
		return noProposeDryRun()
	}
	if _, err := parseWindow(o.window); err != nil {
		return usageErr(fs, err)
	}
	return learnExec(cmd, *o)
}

// learnCommand is a top-level spelling of one learn verb, with only that
// verb's flags.
func learnCommand(name, verb, usage string, args []string) int {
	fs, o := learnFlags(name, usage, verb)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "recall: %s takes no argument %q\n", name, fs.Arg(0))
		fs.Usage()
		return 2
	}
	if verb == "run" && o.dryRun {
		return noProposeDryRun()
	}
	if _, err := parseWindow(o.window); err != nil {
		return usageErr(fs, err)
	}
	return learnExec(verb, *o)
}

func usageErr(fs *flag.FlagSet, err error) int {
	fmt.Fprintf(os.Stderr, "recall: %v\n", err)
	fs.Usage()
	return 2
}

// reduceWindow is --window: days back, or every observation; zero is learn.retire_days.
type reduceWindow struct {
	days int
	all  bool
}

// maxWindowDays keeps the cutoff a real date.
const maxWindowDays = 36500

var windowDaysRE = regexp.MustCompile(`^([0-9]+)d$`)

func parseWindow(s string) (reduceWindow, error) {
	if s == "" {
		return reduceWindow{}, nil
	}
	if s == "all" {
		return reduceWindow{all: true}, nil
	}
	if m := windowDaysRE.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n > maxWindowDays {
			return reduceWindow{}, fmt.Errorf("--window %q: at most %dd; --window all reads every observation", s, maxWindowDays)
		}
		if n > 0 {
			return reduceWindow{days: n}, nil
		}
	}
	return reduceWindow{}, fmt.Errorf("--window %q: want a day count like 365d, or all", s)
}

func noProposeDryRun() int {
	fmt.Fprintln(os.Stderr, "recall: propose has no --dry-run; use recall learn reduce --dry-run, then recall learn apply --dry-run")
	return 2
}

const observeUsage = `Usage:
  recall observe [--since 30d|all] [--limit N] [--idle 6h] [--archives] [--dry-run]
        observe new or changed sessions with learn.map_model; cached in the store.
        --idle skips a session active more recently than that.
        --archives also observes archived weeks from the store.
        The same step as recall learn map.
`

const proposeUsage = `Usage:
  recall propose [--out DIR] [--window 365d|all] [--force]
        sync the rules, fold the window's observations into proposals, and
        send them for review. The window defaults to learn.retire_days;
        --window reads that many days back, or every observation with all.
        --force runs on a machine that isn't the store's proposing machine,
        leaving the store ledger unwritten.
        The same step as recall learn run.
`

const proposalsUsage = `Usage:
  recall proposals
        check what became of sent proposals (merged, closed, pending) and
        list them. The same step as recall learn reviews.
`

const reportUsage = `Usage:
  recall report [--since 30d|all]
        say what recall noticed: the lessons the window's sessions repeat,
        then a report card of your rules; no model call.
        The same step as recall learn report.
`

func cmdObserve(args []string) int { return learnCommand("observe", "map", observeUsage, args) }
func cmdPropose(args []string) int { return learnCommand("propose", "run", proposeUsage, args) }
func cmdProposals(args []string) int {
	return learnCommand("proposals", "reviews", proposalsUsage, args)
}
func cmdReport(args []string) int { return learnCommand("report", "report", reportUsage, args) }

// learnExec runs one learn verb; tests swap it to see what a spelling
// reaches.
var learnExec = execLearn

func execLearn(cmd string, o learnOpts) int {
	env, err := loadArchiveEnv()
	if err != nil {
		return fail(err)
	}
	ctx := context.Background()
	runner, err := env.newRunner()
	if err != nil {
		return fail(err)
	}

	switch cmd {
	case "map":
		return learnMap(ctx, env, runner, o.since, o.limit, o.dryRun, o.archives, o.idle, nil)
	case "observations":
		name := o.harness
		if name == "" {
			name = config.PrimaryHarness(env.settings.Harnesses)
		}
		return learnObservations(ctx, env, name)
	case "candidates":
		return learnCandidates(ctx, env)
	case "report":
		return learnReport(ctx, env, o.since, 0)
	case "rules-sync":
		return learnRulesSync(ctx, env)
	case "targets":
		return learnTargets(ctx, env)
	case "age":
		return learnAge(env)
	case "reduce":
		w, _ := parseWindow(o.window)
		return learnReduce(ctx, env, runner, o.out, o.dryRun, w, o.force)
	case "apply":
		return learnApply(ctx, env, o.from, o.dryRun)
	case "reviews":
		return learnReviews(ctx, env)
	case "reducer":
		return learnReducer(ctx, env, o.take)
	case "run":
		w, _ := parseWindow(o.window)
		code, _ := learnRun(ctx, env, runner, o.out, w, o.force)
		return code
	default:
		fmt.Fprint(os.Stderr, learnUsage)
		return 2
	}
}

// loadRepos finds the context files of each learn.repos checkout, then the
// repo notes under learn.repo_notes. An entry that isn't a git checkout is an
// error, not a skip: a typo here would silently send a repo's lessons to
// the global files.
func loadRepos(ctx context.Context, env *archiveEnv) ([]learn.Repo, error) {
	var repos []learn.Repo
	seen := map[string]bool{}
	for _, path := range env.settings.Learn.Repos {
		root, ok := review.RepoRoot(ctx, path)
		if !ok {
			return nil, fmt.Errorf("learn.repos: %s isn't in a git checkout", tilde(path, env.home))
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		repo, err := learn.DiscoverRepo(root)
		if err != nil {
			return nil, fmt.Errorf("learn.repos: %s: %w", tilde(path, env.home), err)
		}
		repos = append(repos, repo)
	}
	dir := repoNotesDir(env)
	if dir == "" {
		return repos, nil
	}
	if _, ok := review.RepoRoot(ctx, existingAncestor(dir)); !ok {
		return nil, fmt.Errorf("learn.repo_notes: %s isn't in a git checkout, so no review could stage a change to it", tilde(dir, env.home))
	}
	notes, err := learn.DiscoverRepoNotes(dir)
	if err != nil {
		return nil, fmt.Errorf("learn.repo_notes: %w", err)
	}
	return append(repos, notes...), nil
}

// existingAncestor is dir or its nearest parent that exists, so a repo
// notes directory not made yet still finds its checkout.
func existingAncestor(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// repoNotesDir is learn.repo_notes with symlinks resolved; "" when off.
func repoNotesDir(env *archiveEnv) string {
	dir := env.settings.Learn.RepoNotes
	if dir == "" {
		return ""
	}
	return review.Resolve(dir)
}

// evidenceRepos are the repositories an evidence source scans: the origins
// of the learn.repos checkouts, the only repos the loop may read.
func evidenceRepos(repos []learn.Repo) []string {
	var slugs []string
	for _, r := range repos {
		if r.Remote != "" && !r.RepoNote {
			slugs = append(slugs, r.Remote)
		}
	}
	return slugs
}

// ledgerTargets are the files whose bullets are rules: the global targets
// and every existing context file of the listed repos.
func ledgerTargets(env *archiveEnv, repos []learn.Repo) []string {
	targets := append([]string{}, env.settings.Learn.Targets...)
	for _, repo := range repos {
		targets = append(targets, repo.Paths()...)
	}
	return targets
}

func learnTargets(ctx context.Context, env *archiveEnv) int {
	repos, err := loadRepos(ctx, env)
	if err != nil {
		return fail(err)
	}
	for _, t := range env.settings.Learn.Targets {
		fmt.Printf("%-9s  %s\n", "global", tilde(t, env.home))
	}
	for _, repo := range repos {
		kind := "repo"
		if repo.RepoNote {
			kind = "repo note"
		}
		name := repo.Name(env.home)
		for _, f := range repo.Files {
			note := ""
			switch {
			case f.New:
				note = "  (doesn't exist yet)"
			case len(f.Globs) > 0:
				note = "  (paths: " + fmt.Sprint(f.Globs) + ")"
			}
			fmt.Printf("%-9s  %s%s  [%s]\n", kind, tilde(f.Path, env.home), note, name)
		}
	}
	if dir := repoNotesDir(env); dir != "" {
		fmt.Printf("%-9s  %s  (new repo notes go here, one file per repo)\n", "notes", tilde(dir, env.home)+string(filepath.Separator))
	} else {
		fmt.Printf("%-9s  off (learn.repo_notes)\n", "notes")
	}
	return 0
}

// syncedLedger loads the rule ledger against the store's newest version
// (docs/learning.md): on the reducer the local file is authoritative and is
// restored from the store when it is gone; on every other host it is a cache
// of the store's copy, so the rule ids this host shows the model are the
// reducer's.
//
// strict says a failure folding other hosts' review objects fails the run.
// The reduce and apply paths set it: both hold a proposal back when a
// pending review matches it, so a review missed there re-proposes or
// re-publishes a change that is still open for review. Map, rules sync,
// age, and reviews only read rules or refresh verdicts, so there a
// transient store error is a warning and the sync keeps what it read.
func syncedLedger(ctx context.Context, env *archiveEnv, st store.Store, strict bool) (*learn.Ledger, error) {
	reducer, err := storeReducer(ctx, st)
	if err != nil {
		return nil, err
	}
	return learn.SyncLedger(ctx, st, config.LedgerPath(env.home), env.settings.Host, reducer, strict, os.Stderr)
}

// publishStoreLedger writes the ledger to the store as its newest version,
// after a reduce or apply. Only the reducer writes one; a store nobody has
// claimed is claimed first, since the host running the reduce is its
// reducer. On any other host (a forced run), the reviews this run recorded
// go to a per-host store object instead, which the reducer folds into the
// shared ledger at its next sync. Failing to claim or publish fails the
// run, after the caller has saved and printed what it made: the local
// ledger holds the result, and a run that exits clean while the store never
// changes hides a missing grant for weeks.
func publishStoreLedger(ctx context.Context, env *archiveEnv, st store.Store, ledger *learn.Ledger) error {
	fail := func(err error) error {
		return fmt.Errorf("publishing the rule ledger to the store: %w%s", err, grantHint(err))
	}
	host := env.settings.Host
	claim, err := storeReducer(ctx, st)
	if err != nil {
		return fail(err)
	}
	if claim == "" {
		if err := claimReducer(ctx, st, host); err != nil {
			return fail(err)
		}
		if claim, err = storeReducer(ctx, st); err != nil {
			return fail(err)
		}
	}
	if claim != host {
		note := "it recorded no proposal the store ledger lacks"
		if key, perr := learn.PutHostReviews(ctx, st, host, ledger, env.clk.Now()); perr != nil {
			note = fmt.Sprintf("writing this run's proposals for the proposing machine failed (%v); they stay on this host's local ledger until a later run writes them", perr)
		} else if key != "" {
			note = fmt.Sprintf("proposals recorded by this run were written to %s, and the proposing machine folds them into the shared ledger at its next sync", key)
		}
		fmt.Fprintf(os.Stderr, "recall: note: %s is this store's proposing machine and this machine is %s, so this host didn't write the store ledger; %s; "+
			"if %s is gone, `recall learn reducer --take` moves the claim here\n", claim, host, note, claim)
		return nil
	}
	if _, err := learn.PutStoreLedger(ctx, st, ledger, env.clk.Now()); err != nil {
		return fail(err)
	}
	return nil
}

// checkReducerWrites fails a reduce before its model call when this host
// can't write what the reduce ends by writing: the reducer claim on an
// unclaimed store, which it takes now rather than after, and a new ledger
// version. A host that isn't the reducer writes neither.
func checkReducerWrites(ctx context.Context, env *archiveEnv, st store.Store) error {
	fail := func(err error) error {
		return fmt.Errorf("this host reduces for the store but can't write to it: %w%s", err, grantHint(err))
	}
	claim, err := storeReducer(ctx, st)
	if err != nil {
		return nil // reducerGate already warned; the publish reports it
	}
	if claim == "" {
		if err := claimReducer(ctx, st, env.settings.Host); err != nil {
			return fail(err)
		}
		claim = env.settings.Host
	}
	if claim != env.settings.Host {
		return nil
	}
	if err := learn.CheckStoreLedgerWrite(ctx, st); err != nil {
		return fail(err)
	}
	return nil
}

// grantHint points a denied store write at the grants it needs.
func grantHint(err error) string {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "accessdenied") || strings.Contains(msg, "statuscode: 403") || strings.Contains(msg, "permission denied") {
		return "; the reducer needs s3:PutObject on learn/reducer and ledger/* (docs/store.md lists every grant)"
	}
	return ""
}

// loadLedgerAndHits loads the synced ledger with every harness's rule hits
// applied, and returns the observations it read alongside it.
func loadLedgerAndHits(ctx context.Context, env *archiveEnv, st store.Store, strict bool) (*learn.Ledger, []learn.SessionObservations, error) {
	ledger, err := syncedLedger(ctx, env, st, strict)
	if err != nil {
		return nil, nil, err
	}
	undated := 0
	// One ApplyHits call over every harness's hits: it recomputes each
	// rule's dates from all hits for the rule, so a partial batch would
	// drop the other harnesses' dates.
	var window []learn.SessionObservations
	var judged []learn.SessionHits
	for _, h := range env.harnesses {
		w, err := learn.ListObservations(ctx, st, h.Name())
		if err != nil {
			return nil, nil, err
		}
		window = append(window, w...)
		j, err := learn.ListHits(ctx, st, h.Name())
		if err != nil {
			return nil, nil, err
		}
		judged = append(judged, j...)
	}
	ledger.ApplyHits(window, judged)
	for _, so := range window {
		if !so.Dated() {
			undated++
		}
	}
	if undated > 0 {
		fmt.Fprintf(os.Stderr, "recall: warning: %d session(s) were mapped before observations carried the session's own time, "+
			"so their evidence is dated by the day they were observed; `recall observe --since all` reads them again\n", undated)
	}
	return ledger, window, nil
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

// mapRun is one observe pass: the counts it reports and the failure
// streak that stops it. Workers share it under mu.
type mapRun struct {
	mu sync.Mutex
	// mapped sessions were observed; judged ones were only judged against
	// rules, their observations already cached.
	mapped, judged, cached, skipped, failed, partial, streak int
	// chars is the input a dry run's calls would send.
	chars  int
	cancel context.CancelFunc
	// done of total sessions are finished; progress, when set, hears each
	// one, outside mu.
	done, total int
	progress    func(done, total int)
}

// step is the progress prefix of a session's line: how many of the pass's
// sessions are finished, this one included. Called under mu.
func (r *mapRun) step() string {
	if r.total == 0 {
		return ""
	}
	return fmt.Sprintf("[%d/%d] ", r.done, r.total)
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

// datedEvents reports whether any of a session's events carries a
// timestamp; a session with none can't be placed in a bounded window.
func datedEvents(evs []events.Event) bool {
	for _, e := range evs {
		if e.Timestamp != "" {
			return true
		}
	}
	return false
}

// takeSession says whether the map reads a session. One in the window is
// read. One whose events carry no timestamps falls out of every window,
// even --since all, if judged by timestamp alone; so an unbounded window
// (all) takes it, and it maps as undated, while a bounded window skips and
// counts it for the caller's one warning.
func takeSession(evs []events.Event, cutoff string, all bool, undatedSkipped *int) bool {
	if inWindow(evs, cutoff) {
		return true
	}
	if datedEvents(evs) {
		return false
	}
	if all {
		return true
	}
	*undatedSkipped++
	return false
}

func newJob(m *learn.Mapper, harnessName, session string, evs []events.Event) mapJob {
	return mapJob{m: m, harness: harnessName, session: session, evs: evs,
		label: harnessName + "/" + session + " @" + m.Host}
}

// dryRun reports what mapping the job would do, without a model call.
func (r *mapRun) dryRun(ctx context.Context, j mapJob) {
	observe, judge, chars, err := j.m.WouldSend(ctx, j.harness, j.session, j.evs)
	r.chars += chars
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "recall: %s: %v (skipped)\n", j.label, err)
		r.skipped++
	case observe:
		fmt.Printf("would map %s (%d events)\n", j.label, len(j.evs))
		r.mapped++
	case judge:
		fmt.Printf("would judge %s against rules it hasn't seen (%d events)\n", j.label, len(j.evs))
		r.judged++
	default:
		r.cached++
	}
}

// work maps one job and records the result.
func (r *mapRun) work(ctx context.Context, j mapJob) {
	res, err := j.m.MapSession(ctx, j.harness, j.session, j.evs)
	r.mu.Lock()
	r.done++
	done := r.done
	r.record(ctx, j, res, err)
	r.mu.Unlock()
	if r.progress != nil && ctx.Err() == nil {
		r.progress(done, r.total)
	}
}

// record counts and reports one mapped session. Called under mu.
func (r *mapRun) record(ctx context.Context, j mapJob, res *learn.Mapped, err error) {
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
	hits := 0
	if res.Hits != nil {
		hits = len(res.Hits.Hits)
	}
	switch {
	case res.Observed:
		r.mapped++
		note := ""
		if p := res.Observations.Partial; p != nil {
			note = fmt.Sprintf("; %s: read the first %d of %d prompts", p.Reason, p.Prompts, p.Of)
			r.partial++
		}
		fmt.Printf("%sobserved %s: %d observation(s), %d rule hit(s) (%s%s)\n",
			r.step(), j.label, len(res.Observations.Observations), hits, res.Usage, note)
	case res.Judged:
		r.judged++
		fmt.Printf("%sjudged %s: %d rule hit(s) (%s)\n", r.step(), j.label, hits, res.Usage)
	default:
		r.cached++
	}
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

// learnMap maps the window's sessions and reads the evidence sources.
// evidenceGate, when set, says whether a named source is scanned this run;
// the hook worker passes one so a scan's subprocess calls happen at most
// once a day, while a map run by hand (nil) always scans.
func learnMap(ctx context.Context, env *archiveEnv, runner model.Runner, since string, limit int, dryRun, fromArchives bool,
	idle time.Duration, evidenceGate func(name string) bool) int {
	p, err := planMap(ctx, env, runner, since, limit, fromArchives, idle)
	if err != nil {
		return fail(err)
	}
	if dryRun {
		return p.dryRun(ctx)
	}
	return p.execute(ctx, evidenceGate, nil)
}

// mapPass is one observe pass, planned: the sessions it would read and
// what it reads beside them, before any model call.
type mapPass struct {
	env          *archiveEnv
	st           store.Store
	repos        []learn.Repo
	run          *mapRun
	mappers      []*learn.Mapper
	jobs         []mapJob
	notes        []harness.Memory
	noteHarness  map[string]string
	cutoffTime   time.Time
	fromArchives bool
}

// planMap lists the sessions a map over since would read, calling no model.
func planMap(ctx context.Context, env *archiveEnv, runner model.Runner, since string, limit int, fromArchives bool,
	idle time.Duration) (*mapPass, error) {
	cutoffTime, err := parseSince(since, env.clk.Now())
	if err != nil {
		return nil, err
	}
	l := env.settings.Learn
	if err := model.Check(runner, model.Call{Model: l.MapModel, Effort: l.MapEffort}); err != nil {
		return nil, fmt.Errorf("learn.map_effort: %w", err)
	}
	cutoff := cutoffTime.UTC().Format("2006-01-02T15:04:05Z")
	allWindow := cutoffTime.IsZero() // --since all
	undatedSkipped := 0
	st, _, err := openStore(ctx, env)
	if err != nil {
		return nil, err
	}
	// The store's ledger first, so the rule ids this host shows the model
	// are the reducer's, whichever host this is. A map only shows rules, so
	// a failure folding other hosts' reviews degrades to a warning.
	ledger, err := syncedLedger(ctx, env, st, false)
	if err != nil {
		return nil, err
	}
	repos, err := loadRepos(ctx, env)
	if err != nil {
		return nil, err
	}
	scoping := &learn.Scoping{Repos: repos, Home: env.home}
	p := &mapPass{env: env, st: st, repos: repos, run: &mapRun{}, cutoffTime: cutoffTime, fromArchives: fromArchives,
		noteHarness: map[string]string{}}
	mapper := func(host string, h harness.Harness) *learn.Mapper {
		m := &learn.Mapper{
			Store: st, Runner: runner, Model: env.settings.Learn.MapModel, Effort: env.settings.Learn.MapEffort,
			Host: host, Rules: ledger.Rules, Clock: env.clk, Out: os.Stdout, Scoping: scoping,
			Redact: env.settings.Archive.Redact,
		}
		if host == env.settings.Host && env.settings.Harnesses[h.Name()].Format != config.FormatEvents {
			// Only this machine's own sessions name paths that exist here;
			// an events harness's ran elsewhere.
			m.Home, m.Locate = env.home, learn.LocateCheckout(env.home)
		}
		p.mappers = append(p.mappers, m)
		return m
	}
	for _, h := range env.harnesses {
		m := mapper(env.settings.Host, h)
		sessions, err := h.Sessions()
		if err != nil {
			return nil, err
		}
		// Sessions still on disk read from disk; the archives fill in only
		// this host's sessions that are gone (dedupe by session id).
		onDisk := map[string]bool{}
		for _, s := range sessions {
			if id, ok := h.SessionID(s); ok {
				onDisk[id] = true
			}
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
			if idle > 0 && time.Duration(env.clk.Now().UnixNano()-mtimes[s]) < idle {
				continue // still going, or just ended and may be picked up again
			}
			evs, err := h.ReadSessionFile(s, env.settings.Host)
			if err != nil {
				fmt.Fprintf(os.Stderr, "recall: %s: %v (skipped)\n", s, err)
				p.run.skipped++
				continue
			}
			if takeSession(evs, cutoff, allWindow, &undatedSkipped) {
				session, _ := h.SessionID(s)
				p.jobs = append(p.jobs, newJob(m, h.Name(), session, evs))
			}
		}
		if fromArchives {
			archived, err := archiveJobs(ctx, env, st, h, cutoffTime, cutoff,
				func(host string) *learn.Mapper { return mapper(host, h) }, onDisk, &undatedSkipped)
			if err != nil {
				return nil, err
			}
			p.jobs = append(p.jobs, archived...)
		}
	}
	if undatedSkipped > 0 {
		fmt.Fprintf(os.Stderr, "recall: warning: %d session(s) whose events carry no timestamps were skipped, since a bounded window can't place them; `recall observe --since all` reads them\n", undatedSkipped)
	}
	if limit > 0 && len(p.jobs) > limit {
		p.jobs = p.jobs[:limit]
	}
	// Each harness's own memory notes are evidence too, and reading them
	// calls no model.
	for _, h := range env.harnesses {
		mems, err := h.Memories()
		if err != nil {
			return nil, err
		}
		for _, m := range mems {
			p.noteHarness[m.Path] = h.Name()
		}
		p.notes = append(p.notes, mems...)
	}
	return p, nil
}

// calls counts the model calls the pass would make: sessions to observe,
// and sessions already observed that only need judging against rules, and
// the input they would send.
func (p *mapPass) calls(ctx context.Context) (observe, judge, chars int) {
	for _, j := range p.jobs {
		o, jd, n, err := j.m.WouldSend(ctx, j.harness, j.session, j.evs)
		chars += n
		switch {
		case err != nil:
		case o:
			observe++
		case jd:
			judge++
		}
	}
	return observe, judge, chars
}

// dryRun prints what the pass would do and what it would cost.
func (p *mapPass) dryRun(ctx context.Context) int {
	run := p.run
	for _, j := range p.jobs {
		run.dryRun(ctx, j)
	}
	if len(p.notes) > 0 {
		fmt.Printf("observe --dry-run: %d native memory note(s) would be read, with no model call\n", len(p.notes))
	}
	if srcs := p.env.settings.Learn.Evidence; len(srcs) > 0 {
		fmt.Printf("observe --dry-run: evidence source(s) %s would be read, with no model call\n", strings.Join(srcs, ", "))
	}
	fmt.Printf("observe --dry-run: %d session(s) would be observed and %d judged against rules they haven't seen; %d are cached or have no prompts\n",
		run.mapped, run.judged, run.cached)
	if n := run.mapped + run.judged; n > 0 {
		fmt.Printf("observe --dry-run: %s\n", mapEstimate(p.env.settings, n, run.chars, loadedContextChars(p.env.settings.Learn.Runner, p.env.home)))
	}
	return 0
}

// execute runs the pass. progress, when set, is called after each session
// with how many of the pass's sessions are done.
func (p *mapPass) execute(ctx context.Context, evidenceGate func(name string) bool, progress func(done, total int)) int {
	env, st, run := p.env, p.st, p.run
	run.total, run.progress = len(p.jobs), progress
	run.runAll(ctx, p.jobs, env.settings.Learn.MapWorkers)
	added := 0
	for _, h := range env.harnesses {
		var mine []harness.Memory
		for _, m := range p.notes {
			if p.noteHarness[m.Path] == h.Name() {
				mine = append(mine, m)
			}
		}
		n, err := learn.PutMemories(ctx, st, mine, h.Name(), env.settings.Host, env.home, learn.LocateCheckout(env.home), env.clk.Now())
		if err != nil {
			return fail(err)
		}
		added += n
	}
	if len(p.notes) > 0 {
		fmt.Printf("memory: %d native note(s) read, %d new or changed\n", len(p.notes), added)
	}
	// Evidence from outside the chat: review comments and failed checks on
	// agent-authored changes, read with no model call. A source that can't
	// answer (gh missing, offline) is a warning, not a failed map.
	for _, name := range env.settings.Learn.Evidence {
		if evidenceGate != nil && !evidenceGate(name) {
			continue
		}
		src, err := evidence.Open(name, evidence.Options{Repos: evidenceRepos(p.repos), Trailer: env.settings.Learn.EvidenceTrailer, Credentials: env.credentials()})
		if err != nil {
			return fail(err)
		}
		// A source may answer partially: items from the repos that worked,
		// plus an error for the ones that didn't. The items still count.
		items, err := src.Items(ctx, p.cutoffTime)
		if err != nil {
			fmt.Fprintf(os.Stderr, "recall: warning: evidence %s: %v\n", name, err)
			if len(items) == 0 {
				continue
			}
		}
		n, err := learn.PutEvidence(ctx, st, items, src.Name(), env.clk.Now())
		if err != nil {
			return fail(err)
		}
		fmt.Printf("evidence %s: %d item(s) read, %d new or changed pull request(s)\n", name, len(items), n)
	}
	var usage model.Usage
	for _, m := range p.mappers {
		usage = usage.Add(m.Usage)
	}
	fmt.Printf("observe: %d observed, %d cached", run.mapped, run.cached)
	if run.judged > 0 {
		fmt.Printf(", %d judged against rules they hadn't seen", run.judged)
	}
	if run.skipped > 0 {
		fmt.Printf(", %d skipped", run.skipped)
	}
	if run.partial > 0 {
		fmt.Printf(", %d partly read after a refusal", run.partial)
	}
	if run.failed > 0 {
		fmt.Printf(", %d failed", run.failed)
	}
	if !usage.Zero() {
		fmt.Printf(" (%s)", usage)
	}
	fmt.Println()
	if !p.fromArchives {
		warnUnobservedHosts(ctx, env, st)
	}
	if run.failed > 0 {
		return 1
	}
	return 0
}

// mapCallSeconds is about how long one headless map call takes.
const mapCallSeconds = 10

// mapEstimate says about what n map calls cost and how long they take at
// learn.map_workers at once, at the prices in internal/config/prices.json.
// chars is the input the calls send in all; contextChars is the context
// file the runner's CLI loads into each call, beside its own system prompt.
func mapEstimate(s *config.Settings, n, chars, contextChars int) string {
	workers := max(s.Learn.MapWorkers, 1)
	minutes := max((n+workers-1)/workers*mapCallSeconds/60, 1)
	price, ok := config.Prices().Lookup(s.Learn.Runner, s.Learn.MapModel)
	if !ok {
		return fmt.Sprintf("%d map call(s), no price for model %s in prices.json; about %d minute(s) at %d at a time",
			n, s.Learn.MapModel, minutes, workers)
	}
	asOf := ""
	if d := config.Prices().Checked(s.Learn.Runner, s.Learn.MapModel); d != "" {
		asOf = ", prices as of " + d
	}
	perCall := contextChars + config.CLIPromptTokens(s.Learn.Runner)*config.CharsPerToken
	cost, unit := price.Cost(n, chars+n*perCall)
	if unit != "" {
		return fmt.Sprintf("%d map call(s), about %.1f %s (%.1f %s each on %s%s) and %d minute(s) at %d at a time",
			n, cost, unit, price.CreditsPerCall, unit, s.Learn.MapModel, asOf, minutes, workers)
	}
	tokens := (chars + n*perCall) / config.CharsPerToken
	return fmt.Sprintf("%d map call(s), about $%.2f (about %s input tokens on %s at $%g in and $%g out per million%s) and %d minute(s) at %d at a time",
		n, cost, roundTokens(tokens), s.Learn.MapModel, price.InputUSDPerMTok, price.OutputUSDPerMTok, asOf, minutes, workers)
}

// loadedContextChars is the characters of the context file the runner's
// CLI loads into every call (its DefaultTarget); 0 when there is none.
func loadedContextChars(runner, home string) int {
	path := config.DefaultTarget(runner, home)
	if path == "" {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return utf8.RuneCount(b)
}

// roundTokens prints a token count to two significant figures, like 150k.
func roundTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// archiveJobs reads archived sessions from the store, for weeks that overlap
// the window: other hosts' sessions, and this host's own whose transcripts
// are gone from disk (onDisk lists the session ids still there, which keep
// reading from disk). A session archived in more than one week is read from
// its latest week, which holds the whole file.
func archiveJobs(ctx context.Context, env *archiveEnv, st store.Store, h harness.Harness, cutoffTime time.Time,
	cutoff string, mapper func(host string) *learn.Mapper, onDisk map[string]bool, undatedSkipped *int) ([]mapJob, error) {
	keys, err := st.List(ctx, h.Name()+"/")
	if err != nil {
		return nil, err
	}
	byHost := map[string][]string{}
	var hosts []string
	for _, key := range keys {
		if strings.HasSuffix(key, ".age") {
			// Written by the removed client-side encryption feature; name it,
			// never pass it over in silence.
			fmt.Fprintf(os.Stderr, "recall: skipping %s: encrypted by a feature recall removed; decrypt it with the age CLI and upload the plain week\n", key)
			continue
		}
		_, host, w, ok := archive.ParseKey(key)
		if !ok {
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
			if !onDisk[s] && takeSession(sessions[s], cutoff, cutoffTime.IsZero(), undatedSkipped) {
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
				"run `recall observe` there, or `recall observe --archives` here\n", host, h.Name(), unobservedDays)
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

func learnRulesSync(ctx context.Context, env *archiveEnv) int {
	st, _, err := openStore(ctx, env)
	if err != nil {
		return fail(err)
	}
	ledger, err := syncedLedger(ctx, env, st, false)
	if err != nil {
		return fail(err)
	}
	repos, err := loadRepos(ctx, env)
	if err != nil {
		return fail(err)
	}
	res, err := ledger.SyncTargets(ledgerTargets(env, repos), env.clk.Now())
	if err != nil {
		return fail(err)
	}
	if err := ledger.Save(config.LedgerPath(env.home)); err != nil {
		return fail(err)
	}
	fmt.Printf("ledger %s: %d rule(s), %d added, %d removed, %d reworded, %d moved\n",
		tilde(config.LedgerPath(env.home), env.home), len(ledger.Rules), res.Added, res.Removed, res.Reworded, res.Moved)
	return 0
}

func learnAge(env *archiveEnv) int {
	ctx := context.Background()
	st, _, err := openStore(ctx, env)
	if err != nil {
		return fail(err)
	}
	ledger, window, err := loadLedgerAndHits(ctx, env, st, false)
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
	// Rules earned on a model no recent session runs: worth a trial
	// without them, and never a reason to exit nonzero. The ablation picks
	// outgrown rules with the same cutoff.
	for _, r := range ledger.Outgrown(learn.WindowModels(window, env.clk.Now())) {
		fmt.Printf("check %s (earned on %s, which no session in the last %d days ran): %s\n",
			r.ID, strings.Join(r.Models, ", "), learn.RecentModelDays, r.Text)
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

// runDirRE is a reduce run directory's name: the date, with a suffix for a
// day's second and later runs.
var runDirRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(-\d+)?$`)

// freshRunDir is where today's reduce lands: the day's dated directory,
// unless a run already sits there (it holds a proposals.md), in which case
// the day's first free suffixed sibling (-2, -3, ...), the way review ids
// suffix. A second reduce the same day then never overwrites an existing
// run's proposals, applied or still waiting; a following apply picks the
// day's newest unapplied run, so the new proposals are what it stages.
func freshRunDir(root string, now time.Time) string {
	date := now.Format("2006-01-02")
	dir := filepath.Join(root, date)
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(dir, "proposals.md")); err != nil {
			return dir
		}
		dir = filepath.Join(root, fmt.Sprintf("%s-%d", date, n))
	}
}

// runDirOrder is a run directory's place within its day: 1 for the dated
// directory, its suffix for a rerun.
func runDirOrder(dir string) int {
	base := filepath.Base(dir)
	if len(base) <= len("2006-01-02") {
		return 1
	}
	n, _ := strconv.Atoi(base[len("2006-01-02-"):])
	return n
}

// latestRun returns the newest reduce run under root that holds a
// proposals.md: the newest day's run, and within that day the newest rerun
// no apply has finished, since a suffixed sibling holds proposals the
// day's earlier runs don't. An unapplied run from an older day is never
// the answer: the owner saw it and chose not to apply it, and --from
// reaches it explicitly. When every run of the newest day is applied, the
// newest answers, so an apply by hand still says "already applied"
// instead of failing.
func latestRun(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var runs []string
	for _, e := range entries {
		if !e.IsDir() || !runDirRE.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "proposals.md")); err == nil {
			runs = append(runs, dir)
		}
	}
	if len(runs) == 0 {
		return "", fmt.Errorf("no reduce run with a proposals.md under %s; run `recall learn reduce` first", root)
	}
	sort.Slice(runs, func(i, j int) bool {
		di, dj := filepath.Base(runs[i])[:len("2006-01-02")], filepath.Base(runs[j])[:len("2006-01-02")]
		if di != dj {
			return di < dj
		}
		return runDirOrder(runs[i]) < runDirOrder(runs[j])
	})
	day := filepath.Base(runs[len(runs)-1])[:len("2006-01-02")]
	for i := len(runs) - 1; i >= 0 && strings.HasPrefix(filepath.Base(runs[i]), day); i-- {
		if _, err := os.Stat(filepath.Join(runs[i], "apply.json")); err != nil {
			return runs[i], nil
		}
	}
	return runs[len(runs)-1], nil
}

// newReducer loads what a reduce reads: the ledger with its rule hits and
// review verdicts, the candidate ledger, and every host's observations.
// A nil runner is a listing that proposes nothing, so it skips the taken
// names, which cost a harness start. w is --window.
func newReducer(ctx context.Context, env *archiveEnv, st store.Store, runner model.Runner, w reduceWindow) (*learn.Reducer, []learn.SessionObservations, error) {
	if l := env.settings.Learn; runner != nil {
		if err := model.Check(runner, model.Call{Model: l.ReduceModel, Effort: l.ReduceEffort}); err != nil {
			return nil, nil, fmt.Errorf("learn.reduce_effort: %w", err)
		}
		if l.Replay {
			if err := model.Check(runner, model.Call{Model: l.MapModel, Effort: l.MapEffort}); err != nil {
				return nil, nil, fmt.Errorf("learn.map_effort: %w", err)
			}
		}
	}
	// Strict: a reduce proposes against the pending reviews, so a review
	// missed here re-proposes a change that is still open (syncedLedger).
	ledger, window, err := loadLedgerAndHits(ctx, env, st, true)
	if err != nil {
		return nil, nil, err
	}
	refreshReviews(ctx, env, ledger)
	candidates, err := learn.LoadCandidates(config.CandidatesPath(env.home))
	if err != nil {
		return nil, nil, err
	}
	repos, err := loadRepos(ctx, env)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range env.settings.Learn.Evidence {
		w, err := learn.ListObservations(ctx, st, learn.EvidenceHarness(name))
		if err != nil {
			return nil, nil, err
		}
		window = append(window, w...)
	}
	var taken []string
	if runner != nil {
		if taken, err = takenList(ctx, env); err != nil {
			return nil, nil, err
		}
	}
	warnUnobservedHosts(ctx, env, st)
	r := &learn.Reducer{
		Gates:  learnGates(env, repos, taken, ledger),
		Runner: runner, Model: env.settings.Learn.ReduceModel, Effort: env.settings.Learn.ReduceEffort,
		RetireDays: env.settings.Learn.RetireDays,
		WindowDays: w.days, AllWindow: w.all,
		Clock: env.clk, Out: os.Stdout,
		Candidates:        candidates,
		OwnerInstructions: extension.Read(env.settings.Extensions["learn-reduce"], os.Stderr),
	}
	if runner != nil && env.settings.Learn.Replay {
		// Replays run headless with no tools on the map model, the one that
		// reads sessions, in the run's own directory under the state root.
		r.Replay = &learn.Replayer{Runner: runner, Model: env.settings.Learn.MapModel, Effort: env.settings.Learn.MapEffort,
			Load: sessionLoader(env), Out: os.Stderr}
	}
	return r, window, nil
}

// sessionLoader finds one session's events on this machine, for a replay.
// A session from another host, or one whose transcript is gone, isn't here.
func sessionLoader(env *archiveEnv) learn.SessionLoader {
	return func(harnessName, session string) ([]events.Event, bool) {
		for _, h := range env.harnesses {
			if h.Name() != harnessName {
				continue
			}
			sessions, err := h.Sessions()
			if err != nil {
				return nil, false
			}
			for _, s := range sessions {
				if id, ok := h.SessionID(s); ok && id == session {
					evs, err := h.ReadSessionFile(s, env.settings.Host)
					if err != nil {
						return nil, false
					}
					return evs, true
				}
			}
		}
		return nil, false
	}
}

// learnCandidates prints the candidates as the reduce would read them. It
// saves nothing, so new candidates get their ids when a reduce runs.
func learnCandidates(ctx context.Context, env *archiveEnv) int {
	st, _, err := openStore(ctx, env)
	if err != nil {
		return fail(err)
	}
	r, window, err := newReducer(ctx, env, st, nil, reduceWindow{})
	if err != nil {
		return fail(err)
	}
	fmt.Print(r.CandidatesText(window))
	return 0
}

// learnReport prints what recall noticed over since and the rule report
// card, from the observations, rule hits, and ledgers it already stores; limit caps the findings shown (0 for all).
func learnReport(ctx context.Context, env *archiveEnv, since string, limit int) int {
	r, err := buildReport(ctx, env, since)
	if err != nil {
		return fail(err)
	}
	r.Write(os.Stdout, limit)
	return 0
}

// buildReport reads what a report needs: every enabled harness's
// observations and rule hits, the candidate ledger, and the rule ledger.
func buildReport(ctx context.Context, env *archiveEnv, since string) (learn.Report, error) {
	cutoffTime, err := parseSince(since, env.clk.Now())
	if err != nil {
		return learn.Report{}, err
	}
	cutoff := ""
	if !cutoffTime.IsZero() {
		cutoff = cutoffTime.UTC().Format("2006-01-02")
	}
	st, _, err := openStore(ctx, env)
	if err != nil {
		return learn.Report{}, err
	}
	ledger, err := syncedLedger(ctx, env, st, false)
	if err != nil {
		return learn.Report{}, err
	}
	var window []learn.SessionObservations
	var judged []learn.SessionHits
	for _, h := range env.harnesses {
		w, err := learn.ListObservations(ctx, st, h.Name())
		if err != nil {
			return learn.Report{}, err
		}
		window = append(window, w...)
		j, err := learn.ListHits(ctx, st, h.Name())
		if err != nil {
			return learn.Report{}, err
		}
		judged = append(judged, j...)
	}
	candidates, err := learn.LoadCandidates(config.CandidatesPath(env.home))
	if err != nil {
		return learn.Report{}, err
	}
	r := learn.BuildReport(window, judged, ledger, candidates, cutoff, env.clk.Now().Format("2006-01-02"))
	r.Home = env.home
	return r, nil
}

// learnReducer prints or moves the store's reduce claim. Without --take it
// says which host holds the claim; --take overwrites the claim with this
// host, for when the holder is retired, reimaged, or renamed, and this
// host's next reduce then publishes the ledger normally.
func learnReducer(ctx context.Context, env *archiveEnv, take bool) int {
	st, _, err := openStore(ctx, env)
	if err != nil {
		return fail(err)
	}
	claim, err := storeReducer(ctx, st)
	if err != nil {
		return fail(err)
	}
	host := env.settings.Host
	if !take {
		switch claim {
		case "":
			fmt.Println("reducer: unclaimed; the first `recall init` or `recall learn reduce` on this store claims it")
		case host:
			fmt.Printf("reducer: %s (this machine)\n", claim)
		default:
			fmt.Printf("reducer: %s; this machine is %s\n", claim, host)
			fmt.Printf("if %s is gone, `recall learn reducer --take` moves the claim here\n", claim)
		}
		return 0
	}
	if claim == host {
		fmt.Printf("reducer: already %s (this machine)\n", host)
		return 0
	}
	if err := st.Put(ctx, reducerKey, strings.NewReader(host+"\n")); err != nil {
		return fail(err)
	}
	if claim == "" {
		fmt.Printf("reducer: claimed this store for %s\n", host)
	} else {
		fmt.Printf("reducer: took the claim over from %s; %s runs the reduce and writes the store ledger now\n", claim, host)
	}
	return 0
}

// reducerGate refuses a reduce on a machine that isn't the store's reducer,
// unless forced: a reduce here would assign rule ids the store ledger never
// learns. An unclaimed store passes, and the reduce claims it; a store that
// can't be read warns and passes, so an offline hook run never wedges.
func reducerGate(ctx context.Context, env *archiveEnv, st store.Store, force bool) error {
	claim, err := storeReducer(ctx, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: warning: couldn't read which machine proposes for this store (%v); proposing anyway\n", err)
		return nil
	}
	if claim == "" || claim == env.settings.Host || force {
		return nil
	}
	return fmt.Errorf("%s is this store's proposing machine and this machine is %s, so proposing here would assign rule ids the store ledger never learns; "+
		"propose there, run `recall learn reducer --take` here if %s is gone, or pass --force to propose anyway (the store ledger stays unwritten)",
		claim, env.settings.Host, claim)
}

func learnReduce(ctx context.Context, env *archiveEnv, runner model.Runner, outDir string, dryRun bool, w reduceWindow, force bool) int {
	if _, _, err := reduce(ctx, env, runner, outDir, dryRun, w, force); err != nil {
		return fail(err)
	}
	return 0
}

// reduce runs the reduce into outDir and returns that directory; a dry run
// prints the input and returns "". read reports whether the reduce had any
// candidates to read. The store is opened once here and threaded through
// the gate, the reducer, and the ledger publish.
func reduce(ctx context.Context, env *archiveEnv, runner model.Runner, outDir string, dryRun bool, w reduceWindow, force bool) (dir string, read bool, err error) {
	st, _, err := openStore(ctx, env)
	if err != nil {
		return "", false, err
	}
	if err := reducerGate(ctx, env, st, force); err != nil {
		return "", false, err
	}
	r, window, err := newReducer(ctx, env, st, runner, w)
	if err != nil {
		return "", false, err
	}
	if dryRun {
		fmt.Print(r.BuildInput(window))
		return "", false, nil
	}
	if err := checkReducerWrites(ctx, env, st); err != nil {
		return "", false, err
	}
	if outDir == "" {
		outDir = freshRunDir(learnStateRoot(env), env.clk.Now())
	}
	proposals, usage, err := r.Run(ctx, window, outDir)
	if err != nil {
		return "", false, err
	}
	// The ledger's relevance dates moved with the window's rule hits, and
	// the candidates the proposals cited are now used.
	if err := r.Ledger.Save(config.LedgerPath(env.home)); err != nil {
		return "", false, err
	}
	// The store gets the result as a new ledger version, so every other
	// host's next map judges against these rules and ids.
	perr := publishStoreLedger(ctx, env, st, r.Ledger)
	if err := r.Candidates.Save(config.CandidatesPath(env.home)); err != nil {
		return "", false, err
	}
	fmt.Printf("proposals: %s (input beside it) (%s)\n", tilde(proposals, env.home), usage)
	if perr != nil {
		return "", false, perr
	}
	return outDir, r.OpenCandidates > 0, nil
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

// learnGates builds the checks the reduce and the apply share (learn.Gates)
// in one place, from the same config, so both hold a proposal back by the
// same rules. The ledger supplies the rules and reviews the checks read.
func learnGates(env *archiveEnv, repos []learn.Repo, taken []string, ledger *learn.Ledger) learn.Gates {
	l := env.settings.Learn
	// Each enabled harness's files, includes followed, for harness scope.
	readers, own := map[string][]string{}, map[string]string{}
	linter := &lint.Linter{Home: env.home}
	for _, h := range env.harnesses {
		readers[h.Name()] = linter.Reach(h.ContextFiles())
		if t := config.DefaultTarget(h.Name(), env.home); t != "" {
			own[h.Name()] = t
		}
	}
	return learn.Gates{
		Readers: readers, Own: own,
		Targets: l.Targets, Repos: repos, Home: env.home, RepoNotes: repoNotesDir(env),
		NoteHarnesses: config.RepoNoteHarnesses(env.settings.Harnesses),
		Skills:        l.Skills, Bin: l.Bin, Settings: l.Settings,
		Taken: taken, Budget: l.Budget,
		Ledger:                 ledger,
		RequireOwnerEvidence:   !env.noOwnerEvidence,
		RepeatEvidenceSessions: l.RepeatEvidenceSessions,
	}
}

func newApplier(ctx context.Context, env *archiveEnv, ledger *learn.Ledger) (*learn.Applier, error) {
	pub, err := env.publisher(env.settings.Learn.Review)
	if err != nil {
		return nil, fmt.Errorf("learn.review: %w", err)
	}
	taken, err := takenList(ctx, env)
	if err != nil {
		return nil, err
	}
	repos, err := loadRepos(ctx, env)
	if err != nil {
		return nil, err
	}
	l := env.settings.Learn
	editor, err := env.newEditor()
	var runner model.Runner
	if errors.Is(err, model.ErrNoEditor) {
		// Text mode: the runner gets no tools, and the binary writes the
		// files its answer returns.
		editor = nil
		runner, err = env.newRunner()
	}
	if err != nil {
		return nil, err
	}
	var applier any = editor
	if editor == nil {
		applier = runner
	}
	if err := model.Check(applier, model.Call{Model: l.ApplyModel, Effort: l.ApplyEffort}); err != nil {
		return nil, fmt.Errorf("learn.apply_effort: %w", err)
	}
	return &learn.Applier{
		Gates:  learnGates(env, repos, taken, ledger),
		Editor: editor, Runner: runner, Model: l.ApplyModel, Effort: l.ApplyEffort, Publisher: pub,
		Public: env.settings.Keep.Public,
		Check:  l.Check, Clock: env.clk, Out: os.Stderr,
		OwnerInstructions: extension.Read(env.settings.Extensions["learn-apply"], os.Stderr),
	}, nil
}

func learnApply(ctx context.Context, env *archiveEnv, dir string, dryRun bool) int {
	if dir == "" {
		latest, err := latestRun(learnStateRoot(env))
		if err != nil {
			return fail(err)
		}
		dir = latest
	}
	code, _ := applyRun(ctx, env, dir, dryRun)
	return code
}

// applyRun applies one reduce run. done reports whether the run is settled:
// it applied, or there was genuinely nothing to apply; a run an earlier
// apply already finished is not this run's success, so a caller pacing
// itself by the result never counts a dropped run as done.
func applyRun(ctx context.Context, env *archiveEnv, dir string, dryRun bool) (code int, done bool) {
	st, _, err := openStore(ctx, env)
	if err != nil {
		return fail(err), false
	}
	// Strict: an apply publishes proposals the pending reviews don't
	// already cover, so a review missed here re-publishes one (syncedLedger).
	ledger, err := syncedLedger(ctx, env, st, true)
	if err != nil {
		return fail(err), false
	}
	refreshReviews(ctx, env, ledger)
	a, err := newApplier(ctx, env, ledger)
	if err != nil {
		return fail(err), false
	}
	plan, err := a.Plan(ctx, dir)
	if errors.Is(err, learn.ErrApplied) {
		fmt.Printf("apply: %s is already applied\n", tilde(dir, env.home))
		return 0, false
	}
	if err != nil {
		return fail(err), false
	}
	// A day's reruns live in suffixed directories; their branches carry the
	// suffix too, so a rerun never collides with the branch the day's first
	// apply left for review.
	if base := filepath.Base(dir); runDirRE.MatchString(base) {
		plan.Branch = "learn-" + base
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
		return 0, true
	}
	if plan.Ready() == 0 {
		fmt.Println("apply: nothing to apply")
	}
	held := map[string]bool{} // skips printed above; Run adds its own
	for _, r := range plan.Routes {
		if r.Skip != "" {
			held[r.ID] = true
		}
	}
	res, err := a.Run(ctx, plan)
	var perr error
	if res != nil {
		// What went to review is pending until the owner decides; the next
		// reduce and apply read it from the ledger.
		ledger.RecordReviews(plan, res, env.settings.Learn.Review)
		if serr := ledger.Save(config.LedgerPath(env.home)); serr != nil {
			fmt.Fprintf(os.Stderr, "recall: saving the review ledger: %v\n", serr)
		} else {
			// The pending reviews ride the store ledger too, so they survive
			// losing the local file.
			perr = publishStoreLedger(ctx, env, st, ledger)
		}
		for _, o := range res.Outcomes {
			if !o.Applied && !held[o.Route] {
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
	if err := errors.Join(err, perr); err != nil {
		return fail(err), false
	}
	return 0, true
}

// refreshReviews updates the ledger's pending reviews from their
// publishers. A publisher that can't answer is a warning, not a stop: its
// reviews stay pending and hold their proposals back.
func refreshReviews(ctx context.Context, env *archiveEnv, ledger *learn.Ledger) {
	n, err := ledger.RefreshReviews(ctx, env.publisher, env.clk.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: warning: checking proposals: %v\n", err)
	}
	if n > 0 {
		fmt.Printf("proposals: %d decided since the last check\n", n)
	}
}

func learnReviews(ctx context.Context, env *archiveEnv) int {
	st, _, err := openStore(ctx, env)
	if err != nil {
		return fail(err)
	}
	ledger, err := syncedLedger(ctx, env, st, false)
	if err != nil {
		return fail(err)
	}
	refreshReviews(ctx, env, ledger)
	if err := ledger.Save(config.LedgerPath(env.home)); err != nil {
		return fail(err)
	}
	if len(ledger.Reviews) == 0 {
		fmt.Println("proposals: none yet; recall propose records what it sends for review")
		return 0
	}
	for _, rv := range ledger.Reviews {
		when := "since " + rv.Proposed
		if rv.Verdict != review.Pending {
			when = rv.Decided
		}
		rv.Repo = tilde(rv.Repo, env.home)
		fmt.Printf("%-7s %s  %s: %s  (%s)\n", rv.Verdict, when, rv.Kind, rv.Title, rv.Place())
	}
	return 0
}

// learnRun is the scheduled loop on one host: put new or hand-edited rules
// on the ledger, reduce, and apply what the reduce proposed. done reports
// whether the run settled its period: the reduce had candidates to read,
// and what it proposed applied or there was nothing to apply. A run that
// reduced nothing, or whose proposals were dropped on an already-applied
// directory, shouldn't count as a period's pass.
func learnRun(ctx context.Context, env *archiveEnv, runner model.Runner, outDir string, w reduceWindow, force bool) (code int, done bool) {
	if code := learnRulesSync(ctx, env); code != 0 {
		return code, false
	}
	dir, read, err := reduce(ctx, env, runner, outDir, false, w, force)
	if err != nil {
		return fail(err), false
	}
	md, err := os.ReadFile(filepath.Join(dir, "proposals.md"))
	if err != nil {
		return fail(err), false
	}
	n := len(learn.ParseProposals(string(md)))
	if n == 0 {
		fmt.Println("run: no proposals this time")
		return 0, read
	}
	code, applied := applyRun(ctx, env, dir, false)
	return code, read && applied
}

func printJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
