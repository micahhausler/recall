package learn

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/review"
	"github.com/micahhausler/recall/internal/store"
)

func peekReview(title string) Review {
	return Review{Title: title, Where: "w-" + title, Kind: "add", Verdict: review.Pending}
}

func reviewTitles(l *Ledger) []string {
	var out []string
	for _, rv := range l.Reviews {
		out = append(out, rv.Title)
	}
	sort.Strings(out)
	return out
}

func storeObjects(t *testing.T, st store.Store) map[string]string {
	t.Helper()
	ctx := context.Background()
	keys, err := st.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, k := range keys {
		r, err := st.Get(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(r)
		r.Close()
		m[k] = string(data)
	}
	return m
}

// PeekLedger loads what SyncLedger would, and writes nothing.
func TestPeekLedgerMatchesSyncLedgerReadOnly(t *testing.T) {
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		host        string
		local       *Ledger
		stored      *Ledger
		hostReviews []Review
		want        []string
	}{
		"proposing machine keeps a newer local copy and folds host reviews": {
			host:        "laptop-a",
			local:       &Ledger{Reviews: []Review{peekReview("a"), peekReview("b")}},
			stored:      &Ledger{Reviews: []Review{peekReview("a")}},
			hostReviews: []Review{peekReview("c")},
			want:        []string{"a", "b", "c"},
		},
		"proposing machine restores an empty local copy": {
			host:   "laptop-a",
			local:  nil,
			stored: &Ledger{Reviews: []Review{peekReview("a")}},
			want:   []string{"a"},
		},
		"proposing machine adopts the store over an older local copy": {
			host:   "laptop-a",
			local:  &Ledger{Reviews: []Review{peekReview("x")}},
			stored: &Ledger{Reviews: []Review{peekReview("a"), peekReview("b")}},
			want:   []string{"a", "b"},
		},
		"another machine merges its own reviews into the store copy": {
			host:   "laptop-b",
			local:  &Ledger{Reviews: []Review{peekReview("mine")}},
			stored: &Ledger{Reviews: []Review{peekReview("a")}},
			want:   []string{"a", "mine"},
		},
		"no store version keeps the local copy": {
			host:  "laptop-b",
			local: &Ledger{Reviews: []Review{peekReview("mine")}},
			want:  []string{"mine"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMem()
			path := filepath.Join(t.TempDir(), "ledger.json")
			if c.local != nil {
				if err := c.local.Save(path); err != nil {
					t.Fatal(err)
				}
			}
			if c.stored != nil {
				if _, err := PutStoreLedger(ctx, st, c.stored, now); err != nil {
					t.Fatal(err)
				}
			}
			if len(c.hostReviews) > 0 {
				if _, err := PutHostReviews(ctx, st, "laptop-b", &Ledger{Reviews: c.hostReviews}, now); err != nil {
					t.Fatal(err)
				}
			}
			file, _ := os.ReadFile(path)
			objects := storeObjects(t, st)

			got, err := PeekLedger(ctx, st, path, c.host, "laptop-a")
			if err != nil {
				t.Fatal(err)
			}
			if titles := reviewTitles(got); !reflect.DeepEqual(titles, c.want) {
				t.Errorf("peek = %v, want %v", titles, c.want)
			}
			if after, _ := os.ReadFile(path); string(after) != string(file) {
				t.Errorf("peek changed the local ledger")
			}
			if after := storeObjects(t, st); !reflect.DeepEqual(after, objects) {
				t.Errorf("peek changed the store")
			}

			synced, err := SyncLedger(ctx, st, path, c.host, "laptop-a", false, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if titles := reviewTitles(synced); !reflect.DeepEqual(titles, c.want) {
				t.Errorf("sync = %v, peek's want %v", titles, c.want)
			}
		})
	}
}
