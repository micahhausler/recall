package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/learn"
)

// A reduce claims the store and publishes the rule ledger as its newest
// version, and after the local ledger file is deleted, the next run restores
// ids and reviews from the store instead of starting over at r-0001 with no
// history.
func TestReducePublishesLedgerAndARunRestoresIt(t *testing.T) {
	f := newHookFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(f.home, ".claude", "CLAUDE.md"),
		[]byte("# Rules\n\n- Run go vet before saying a change is done.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := learnRulesSync(ctx, f.env); code != 0 {
		t.Fatalf("rules sync = %d", code)
	}
	// A review is the part of the ledger nothing else can rebuild.
	ledger, err := learn.LoadLedger(f.env.settings.Learn.Ledger)
	if err != nil || len(ledger.Rules) != 1 {
		t.Fatalf("ledger after sync: %+v, %v", ledger, err)
	}
	ruleID := ledger.Rules[0].ID
	ledger.Reviews = append(ledger.Reviews, learn.Review{
		Kind: "add", Title: "Run go vet before done", Rule: ledger.Rules[0].Text,
		Proposed: "2026-11-12", Via: "branch", Verdict: "merged", Decided: "2026-11-13",
	})
	if err := ledger.Save(f.env.settings.Learn.Ledger); err != nil {
		t.Fatal(err)
	}

	if _, _, err := reduce(ctx, f.env, f.runner, "", false, false, false); err != nil {
		t.Fatal(err)
	}
	st, _, err := openStore(ctx, f.env)
	if err != nil {
		t.Fatal(err)
	}
	if host, err := storeReducer(ctx, st); err != nil || host != "laptop-a" {
		t.Fatalf("reducer claim = %q, %v; the reduce should claim an unclaimed store", host, err)
	}
	keys, err := st.List(ctx, "ledger/")
	if err != nil || len(keys) == 0 {
		t.Fatalf("store ledger versions = %v, %v; the reduce should publish one", keys, err)
	}

	if err := os.Remove(f.env.settings.Learn.Ledger); err != nil {
		t.Fatal(err)
	}
	if code := learnRulesSync(ctx, f.env); code != 0 {
		t.Fatalf("rules sync after losing the file = %d", code)
	}
	restored, err := learn.LoadLedger(f.env.settings.Learn.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Rules) != 1 || restored.Rules[0].ID != ruleID {
		t.Fatalf("restored rules = %+v, want the store's id %s", restored.Rules, ruleID)
	}
	if len(restored.Reviews) == 0 || restored.Reviews[len(restored.Reviews)-1].Title != "Run go vet before done" {
		t.Fatalf("restored reviews = %+v; the store copy carries them", restored.Reviews)
	}
}

// A reducer restored from an old backup runs its reduce against the store's
// newer ledger, not over it: the stale local file is replaced before
// anything publishes, so the store's newest version stands.
func TestReducerRestoredFromBackupAdoptsStoreLedger(t *testing.T) {
	f := newHookFixture(t)
	ctx := context.Background()
	st, _, err := openStore(ctx, f.env)
	if err != nil {
		t.Fatal(err)
	}
	if err := claimReducer(ctx, st, "laptop-a"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.home, ".claude", "CLAUDE.md")
	newer := &learn.Ledger{
		Rules: []learn.Rule{{ID: "r-0003", Target: target,
			Text: "Run go vet before saying a change is done.", Created: "2026-10-01"}},
		Reviews:  []learn.Review{{Kind: "add", Title: "Run go vet before done", Verdict: "merged", Decided: "2026-11-01"}},
		LastRule: 3,
	}
	key, err := learn.PutStoreLedger(ctx, st, newer, f.clk.T.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	stale := &learn.Ledger{Rules: []learn.Rule{{ID: "r-0001", Target: target,
		Text: "An old rule from the backup.", Created: "2026-09-01"}}, LastRule: 1}
	if err := stale.Save(f.env.settings.Learn.Ledger); err != nil {
		t.Fatal(err)
	}

	if _, _, err := reduce(ctx, f.env, f.runner, "", false, false, false); err != nil {
		t.Fatal(err)
	}

	got, newest, err := learn.LoadStoreLedger(ctx, st)
	if err != nil || newest != key || got.LastRule != 3 {
		t.Fatalf("store newest = %q LastRule=%d err=%v; want the pre-backup version %q untouched", newest, got.LastRule, err, key)
	}
	local, err := learn.LoadLedger(f.env.settings.Learn.Ledger)
	if err != nil || local.LastRule != 3 || len(local.Rules) != 1 || local.Rules[0].ID != "r-0003" {
		t.Fatalf("local ledger = %+v, %v; want the store's version adopted", local, err)
	}
}
