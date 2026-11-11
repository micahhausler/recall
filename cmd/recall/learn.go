package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/micahhausler/recall/internal/learn"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/store"
)

const learnUsage = `Usage:
  recall learn map [--since 30d] [--limit N] [--dry-run]
        observe new or changed sessions with the map model; cached in the store
  recall learn observations [--harness claude]
        print the cached observations as JSONL
  recall learn rules sync
        reconcile the rule ledger with the bullets in learn.targets
  recall learn age
        report rules the ledger says to reword or retire; exit 1 if any
  recall learn reduce [--out DIR] [--dry-run]
        fold the window's observations and the ledger into proposals for review

The map and reduce steps call a model through the harness CLI (learn.runner,
default claude -p); --dry-run builds and prints what would be sent instead.
Proposals are written for review, never applied to live files.
`

func cmdLearn(args []string) int {
	fs := flag.NewFlagSet("learn", flag.ContinueOnError)
	since := fs.String("since", "30d", "map: only sessions with events in this window")
	limit := fs.Int("limit", 0, "map: at most this many sessions, newest first")
	outDir := fs.String("out", "", "reduce: where proposals land (default ~/.local/state/recall/learn/<date>)")
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
		return learnMap(ctx, env, runner, *since, *limit, *dryRun)
	case "observations":
		return learnObservations(ctx, env, *harnessName)
	case "rules-sync":
		return learnRulesSync(env)
	case "age":
		return learnAge(env)
	case "reduce":
		return learnReduce(ctx, env, runner, *outDir, *dryRun)
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

func learnMap(ctx context.Context, env *archiveEnv, runner model.Runner, since string, limit int, dryRun bool) int {
	cutoff, err := parseSince(since, env.clk.Now())
	if err != nil {
		return fail(err)
	}
	st, _, err := openStore(ctx, env)
	if err != nil {
		return fail(err)
	}
	ledger, err := learn.LoadLedger(env.settings.Learn.Ledger)
	if err != nil {
		return fail(err)
	}
	mapped, cached, skipped := 0, 0, 0
	var totalUsage model.Usage
	for _, h := range env.harnesses {
		m := &learn.Mapper{
			Store: st, Runner: runner, Model: env.settings.Learn.MapModel,
			Host: env.settings.Host, Rules: ledger.Rules, Clock: env.clk, Out: os.Stdout,
		}
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
			if limit > 0 && mapped+cached >= limit {
				break
			}
			evs, err := h.ReadSessionFile(s, env.settings.Host)
			if err != nil {
				fmt.Fprintf(os.Stderr, "recall: %s: %v (skipped)\n", s, err)
				skipped++
				continue
			}
			recent := false
			for _, e := range evs {
				if e.Timestamp >= cutoff.UTC().Format("2006-01-02T15:04:05Z") {
					recent = true
					break
				}
			}
			if !recent {
				continue
			}
			session := filepath.Base(s)
			session = session[:len(session)-len(filepath.Ext(session))]
			if dryRun {
				fmt.Printf("would map %s/%s (%d events)\n", h.Name(), session, len(evs))
				mapped++
				continue
			}
			so, ran, err := m.MapSession(ctx, h.Name(), session, evs)
			if err != nil {
				return fail(err) // a model error stops the pipeline
			}
			if ran {
				mapped++
				fmt.Printf("observed %s/%s: %d observation(s), %d rule hit(s) ($%.4f)\n",
					h.Name(), session, len(so.Observations), len(so.RuleHits), so.CostUSD)
			} else {
				cached++
			}
		}
		totalUsage = totalUsage.Add(m.Usage)
	}
	fmt.Printf("map: %d observed, %d cached", mapped, cached)
	if skipped > 0 {
		fmt.Printf(", %d skipped", skipped)
	}
	if totalUsage.CostUSD > 0 || totalUsage.InputTokens > 0 {
		fmt.Printf(" (%s)", totalUsage)
	}
	fmt.Println()
	return 0
}

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

func learnReduce(ctx context.Context, env *archiveEnv, runner model.Runner, outDir string, dryRun bool) int {
	st, _, err := openStore(ctx, env)
	if err != nil {
		return fail(err)
	}
	ledger, err := loadLedgerAndHits(ctx, env, st)
	if err != nil {
		return fail(err)
	}
	var window []learn.SessionObservations
	for _, h := range env.harnesses {
		w, err := learn.ListObservations(ctx, st, h.Name())
		if err != nil {
			return fail(err)
		}
		window = append(window, w...)
	}
	r := &learn.Reducer{
		Runner: runner, Model: env.settings.Learn.ReduceModel, Ledger: ledger,
		RetireDays: env.settings.Learn.RetireDays, Clock: env.clk, Out: os.Stdout,
	}
	if dryRun {
		fmt.Print(r.BuildInput(window))
		return 0
	}
	if outDir == "" {
		outDir = filepath.Join(env.home, ".local", "state", "recall", "learn",
			env.clk.Now().Format("2006-01-02"))
	}
	proposals, usage, err := r.Run(ctx, window, outDir)
	if err != nil {
		return fail(err)
	}
	// The ledger's relevance dates moved with the window's rule hits.
	if err := ledger.Save(env.settings.Learn.Ledger); err != nil {
		return fail(err)
	}
	fmt.Printf("proposals: %s (input beside it) (%s)\n", tilde(proposals, env.home), usage)
	return 0
}

func printJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
