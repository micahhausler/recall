package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/store"
)

// deniedPrefixes are the keys the "denying" store refuses to write, the way
// S3 refuses a key the caller's IAM policy doesn't grant.
var deniedPrefixes []string

func init() {
	store.Register("denying", func(ctx context.Context, s config.StoreSettings) (store.Store, error) {
		fs, err := store.NewFS(s.Path)
		if err != nil {
			return nil, err
		}
		return denyingStore{fs}, nil
	})
}

type denyingStore struct{ *store.FS }

func (d denyingStore) denied(key string) error {
	for _, p := range deniedPrefixes {
		if strings.HasPrefix(key, p) {
			return fmt.Errorf("operation error S3: PutObject, https response error StatusCode: 403, api error AccessDenied: not authorized on %s", key)
		}
	}
	return nil
}

func (d denyingStore) PutIfAbsent(ctx context.Context, key string, r io.Reader) error {
	if err := d.denied(key); err != nil {
		return err
	}
	return d.FS.PutIfAbsent(ctx, key, r)
}

func (d denyingStore) Put(ctx context.Context, key string, r io.Reader) error {
	if err := d.denied(key); err != nil {
		return err
	}
	return d.FS.Put(ctx, key, r)
}

// A reducer that can't write the claim or a ledger version fails the
// reduce before the model call, naming the grant, instead of warning after
// it and leaving the store unchanged run after run.
func TestReduceFailsBeforeTheModelWhenReducerWritesAreDenied(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		denied []string
		// published runs one good reduce first, so a ledger version exists
		// to probe and the claim is already held.
		published bool
	}{
		{"allowed", nil, false},
		{"claim", []string{"learn/"}, false},
		{"ledger", []string{"ledger/"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookFixture(t)
			f.env.settings.Store.Kind = "denying"
			deniedPrefixes = nil
			t.Cleanup(func() { deniedPrefixes = nil })
			if tc.published {
				if _, _, err := reduce(ctx, f.env, f.runner, "", false, reduceWindow{}, false); err != nil {
					t.Fatalf("first reduce: %v", err)
				}
			}
			deniedPrefixes = tc.denied
			before := len(f.runner.calls)
			_, _, err := reduce(ctx, f.env, f.runner, "", false, reduceWindow{}, false)
			if tc.denied == nil {
				// The control: the same reduce, unblocked, calls the model.
				if err != nil || len(f.runner.calls) == before {
					t.Fatalf("allowed reduce: err=%v, model calls=%d", err, len(f.runner.calls)-before)
				}
				return
			}
			if err == nil {
				t.Fatal("reduce succeeded with the reducer's writes denied")
			}
			if !strings.Contains(err.Error(), "docs/store.md") {
				t.Errorf("error = %v; want it to point at the grants", err)
			}
			if n := len(f.runner.calls) - before; n != 0 {
				t.Errorf("reduce made %d model call(s) before failing", n)
			}
		})
	}
}
