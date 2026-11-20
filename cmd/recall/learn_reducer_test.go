package main

import (
	"context"
	"strings"
	"testing"
)

// `recall learn reducer --take` moves the store's claim to this host, so a
// retired, reimaged, or renamed machine doesn't strand the ledger; without
// the flag it only reports who holds the claim.
func TestLearnReducerTakeMovesClaim(t *testing.T) {
	f := newHookFixture(t)
	ctx := context.Background()
	st, _, err := openStore(ctx, f.env)
	if err != nil {
		t.Fatal(err)
	}
	if err := claimReducer(ctx, st, "old-laptop"); err != nil {
		t.Fatal(err)
	}
	if code := learnReducer(ctx, f.env, false); code != 0 {
		t.Fatalf("learn reducer = %d", code)
	}
	if host, err := storeReducer(ctx, st); err != nil || host != "old-laptop" {
		t.Fatalf("a status check moved the claim: %q, %v", host, err)
	}
	if code := learnReducer(ctx, f.env, true); code != 0 {
		t.Fatalf("learn reducer --take = %d", code)
	}
	if host, err := storeReducer(ctx, st); err != nil || host != "laptop-a" {
		t.Fatalf("claim after --take = %q, %v; want laptop-a", host, err)
	}
	// Taking what this host already holds changes nothing and succeeds.
	if code := learnReducer(ctx, f.env, true); code != 0 {
		t.Fatalf("learn reducer --take again = %d", code)
	}
	if host, err := storeReducer(ctx, st); err != nil || host != "laptop-a" {
		t.Fatalf("claim after a second --take = %q, %v; want laptop-a", host, err)
	}
}

// A reduce on a machine that isn't the store's reducer refuses, naming the
// claim holder, this host, and the --take hint; --force runs it the old
// way, with a local save and no store ledger write.
func TestReduceRefusesOffTheReducer(t *testing.T) {
	f := newHookFixture(t)
	ctx := context.Background()
	st, _, err := openStore(ctx, f.env)
	if err != nil {
		t.Fatal(err)
	}
	if err := claimReducer(ctx, st, "old-laptop"); err != nil {
		t.Fatal(err)
	}
	_, _, err = reduce(ctx, f.env, f.runner, "", false, false, false)
	if err == nil {
		t.Fatal("a reduce off the reducer ran")
	}
	for _, want := range []string{"old-laptop", "laptop-a", "recall learn reducer --take"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q doesn't name %q", err, want)
		}
	}
	if _, _, err := reduce(ctx, f.env, f.runner, "", false, false, true); err != nil {
		t.Fatalf("forced reduce: %v", err)
	}
	if keys, err := st.List(ctx, "ledger/"); err != nil || len(keys) != 0 {
		t.Fatalf("a forced reduce off the reducer wrote the store ledger: %v, %v", keys, err)
	}
}
