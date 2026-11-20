package learn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/review"
	"github.com/micahhausler/recall/internal/store"
)

func storeRule() Rule {
	return Rule{
		ID: "r-0001", Target: "/home/you/.claude/base.md",
		Text:     "Run go vet before saying a change is done.",
		TextHash: textHash("Run go vet before saying a change is done."),
		Created:  "2026-10-01",
		Supersedes: []Version{{
			TextHash: textHash("Run vet."), Text: "Run vet.", Until: "2026-10-20",
		}},
	}
}

func TestStoreLedgerNewestWinsAndNeverOverwrites(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	l1 := &Ledger{Rules: []Rule{storeRule()}, LastRule: 1}
	key1, err := PutStoreLedger(ctx, st, l1, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	// The same content again writes nothing new.
	again, err := PutStoreLedger(ctx, st, l1, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || again != key1 {
		t.Fatalf("same content: key=%q err=%v, want %q", again, err, key1)
	}
	l2 := &Ledger{Rules: []Rule{storeRule()}, LastRule: 2}
	key2, err := PutStoreLedger(ctx, st, l2, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if key2 <= key1 {
		t.Fatalf("a later version must sort after an earlier one: %q then %q", key1, key2)
	}
	keys, err := st.List(ctx, "ledger/")
	if err != nil || len(keys) != 2 {
		t.Fatalf("versions = %v, %v; want the two distinct contents", keys, err)
	}
	got, key, err := LoadStoreLedger(ctx, st)
	if err != nil || key != key2 || got.LastRule != 2 {
		t.Fatalf("newest = %q LastRule=%d err=%v, want %q LastRule=2", key, got.LastRule, err, key2)
	}
	// Two writes in the same instant get distinct keys, newest content last.
	same := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	l3 := &Ledger{Rules: []Rule{storeRule()}, LastRule: 3}
	l4 := &Ledger{Rules: []Rule{storeRule()}, LastRule: 4}
	key3, err := PutStoreLedger(ctx, st, l3, same)
	if err != nil {
		t.Fatal(err)
	}
	key4, err := PutStoreLedger(ctx, st, l4, same)
	if err != nil {
		t.Fatal(err)
	}
	if key4 <= key3 {
		t.Fatalf("same-instant versions out of order: %q then %q", key3, key4)
	}
	if got, _, _ := LoadStoreLedger(ctx, st); got.LastRule != 4 {
		t.Fatalf("newest LastRule = %d, want 4", got.LastRule)
	}
}

// Done-when for the shared ledger: with two hosts on one store, a hit mapped
// on host B lands on the same rule host A's ledger holds, because B judged
// against the store ledger A published.
func TestHitMappedOnAnotherHostUpdatesSameRule(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	rule := storeRule()
	hostA := &Ledger{Rules: []Rule{rule}, LastRule: 1}
	if _, err := PutStoreLedger(ctx, st, hostA, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	// Host B maps a session against the store ledger's rules.
	pathB := filepath.Join(t.TempDir(), "ledger.json")
	synced, err := SyncLedger(ctx, st, pathB, "laptop-b", "laptop-a", false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(synced.Rules) != 1 || synced.Rules[0].ID != rule.ID {
		t.Fatalf("host B synced rules = %+v", synced.Rules)
	}
	m := mapper(st, &model.Fake{Response: "NONE\nRule[r-0001]: relevant\n"}, synced.Rules)
	m.Host = "laptop-b"
	evs := []events.Event{{Harness: "claude", Session: "s-b", Timestamp: "2026-11-05T10:00:00Z",
		Kind: events.UserPrompt, Text: "check the vet output"}}
	if _, err := m.MapSession(ctx, "claude", "s-b", evs); err != nil {
		t.Fatal(err)
	}

	// Host A folds every host's stored hits into its ledger.
	window, err := ListObservations(ctx, st, "claude")
	if err != nil {
		t.Fatal(err)
	}
	judged, err := ListHits(ctx, st, "claude")
	if err != nil {
		t.Fatal(err)
	}
	hostA.ApplyHits(window, judged)
	if got := hostA.Rules[0].LastRelevant; got != "2026-11-05" {
		t.Fatalf("last_relevant on host A = %q, want 2026-11-05 from host B's hit", got)
	}
}

// Done-when for the cache: after the local ledger file is deleted, a sync
// restores ids, lineage, and reviews from the store, on the reducer and on
// any other host.
func TestSyncLedgerRestoresDeletedLocalFile(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	published := &Ledger{
		Rules:    []Rule{storeRule()},
		Reviews:  []Review{{Kind: "add", Title: "Run go vet before done", Verdict: "pending", Proposed: "2026-11-01"}},
		LastRule: 1,
	}
	if _, err := PutStoreLedger(ctx, st, published, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"laptop-a", "laptop-b"} {
		path := filepath.Join(t.TempDir(), "ledger.json")
		got, err := SyncLedger(ctx, st, path, host, "laptop-a", false, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		onDisk, err := LoadLedger(path)
		if err != nil {
			t.Fatal(err)
		}
		for name, l := range map[string]*Ledger{"returned": got, "restored file": onDisk} {
			switch {
			case len(l.Rules) != 1 || l.Rules[0].ID != "r-0001":
				t.Errorf("%s: %s ledger lost the rule id: %+v", host, name, l.Rules)
			case len(l.Rules[0].Supersedes) != 1:
				t.Errorf("%s: %s ledger lost the lineage: %+v", host, name, l.Rules[0])
			case len(l.Reviews) != 1 || l.Reviews[0].Title != "Run go vet before done":
				t.Errorf("%s: %s ledger lost the reviews: %+v", host, name, l.Reviews)
			}
		}
	}
}

func TestSyncLedgerReplacesAnotherHostsDivergentLedger(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	if _, err := PutStoreLedger(ctx, st, &Ledger{Rules: []Rule{storeRule()}, LastRule: 1},
		time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	// Host B assigned its own ids before the store ledger existed.
	path := filepath.Join(t.TempDir(), "ledger.json")
	divergent := &Ledger{Rules: []Rule{{ID: "r-0001", Target: "/home/you/.claude/base.md",
		Text: "Something else entirely.", TextHash: textHash("Something else entirely."), Created: "2026-09-01"}}, LastRule: 1}
	if err := divergent.Save(path); err != nil {
		t.Fatal(err)
	}
	var note bytes.Buffer
	got, err := SyncLedger(ctx, st, path, "laptop-b", "laptop-a", false, &note)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rules[0].Text != storeRule().Text {
		t.Fatalf("host B kept its divergent rule: %+v", got.Rules[0])
	}
	onDisk, err := LoadLedger(path)
	if err != nil || onDisk.Rules[0].Text != storeRule().Text {
		t.Fatalf("the local cache wasn't replaced: %+v, %v", onDisk.Rules, err)
	}
	if !bytes.Contains(note.Bytes(), []byte("replaced this host's rule ledger")) {
		t.Errorf("no note about the replacement: %q", note.String())
	}
	// The reducer's own divergence is not replaced: its local file is
	// authoritative until it publishes.
	if err := divergent.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err = SyncLedger(ctx, st, path, "laptop-a", "laptop-a", true, io.Discard)
	if err != nil || got.Rules[0].Text != "Something else entirely." {
		t.Fatalf("the reducer's local ledger lost to the store copy: %+v, %v", got.Rules, err)
	}
}

// A reducer restored from an old backup must not publish its stale ledger
// over the store's newer one: the sync adopts the store version, since rule
// numbers and reviews only grow.
func TestSyncLedgerOnReducerAdoptsNewerStoreVersion(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	newer := &Ledger{
		Rules:    []Rule{storeRule()},
		Reviews:  []Review{{Kind: "add", Title: "Run go vet before done", Verdict: "merged", Decided: "2026-11-02"}},
		LastRule: 3,
	}
	key, err := PutStoreLedger(ctx, st, newer, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ledger.json")
	stale := &Ledger{Rules: []Rule{storeRule()}, LastRule: 1}
	if err := stale.Save(path); err != nil {
		t.Fatal(err)
	}
	var note bytes.Buffer
	got, err := SyncLedger(ctx, st, path, "laptop-a", "laptop-a", true, &note)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastRule != 3 || len(got.Reviews) != 1 {
		t.Fatalf("synced ledger = LastRule %d, %d review(s); want the store's", got.LastRule, len(got.Reviews))
	}
	onDisk, err := LoadLedger(path)
	if err != nil || onDisk.LastRule != 3 {
		t.Fatalf("the local file wasn't replaced: %+v, %v", onDisk, err)
	}
	if !bytes.Contains(note.Bytes(), []byte("older")) {
		t.Errorf("no note naming the stale local ledger: %q", note.String())
	}
	// Publishing the adopted ledger writes no new version.
	again, err := PutStoreLedger(ctx, st, got, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC))
	if err != nil || again != key {
		t.Fatalf("publish after adopting = %q, %v; want the existing version %q", again, err, key)
	}
}

// A forced reduce and apply on a non-reducer host records pending reviews
// locally that the store ledger lacks, since only the reducer writes the
// store. Adopting the store's newer version must keep them: losing one
// untracks the published change and re-proposes it while still open.
func TestSyncLedgerKeepsLocalReviewsStoreLacks(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	storedReview := Review{Kind: "add", Title: "Run go vet before done", Via: "pr",
		Where: "https://example.com/you/dotfiles/pull/1", Repo: "/home/you/dotfiles",
		Verdict: "merged", Decided: "2026-11-02"}
	stored := &Ledger{Rules: []Rule{storeRule()}, Reviews: []Review{storedReview}, LastRule: 2}
	if _, err := PutStoreLedger(ctx, st, stored, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	// Host B's forced run published a proposal and recorded it on a local
	// cache that doesn't hold the store's merged review yet.
	localOnly := Review{Kind: "add", Title: "Prefer table tests", Via: "pr",
		Where: "https://example.com/you/dotfiles/pull/2", Repo: "/home/you/dotfiles",
		Verdict: "pending", Proposed: "2026-11-04"}
	path := filepath.Join(t.TempDir(), "ledger.json")
	local := &Ledger{Rules: []Rule{storeRule()}, Reviews: []Review{localOnly}, LastRule: 2}
	if err := local.Save(path); err != nil {
		t.Fatal(err)
	}
	var note bytes.Buffer
	got, err := SyncLedger(ctx, st, path, "laptop-b", "laptop-a", false, &note)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, l := range map[string]*Ledger{"returned": got, "saved file": onDisk} {
		byTitle := map[string]Review{}
		for _, rv := range l.Reviews {
			byTitle[rv.Title] = rv
		}
		if len(l.Reviews) != 2 {
			t.Fatalf("%s ledger holds %d review(s) %v, want the store's and the local pending one", name, len(l.Reviews), byTitle)
		}
		if rv := byTitle[storedReview.Title]; rv.Verdict != "merged" {
			t.Errorf("%s ledger lost the store's review: %+v", name, rv)
		}
		if rv := byTitle[localOnly.Title]; rv.Verdict != "pending" || rv.Where != localOnly.Where {
			t.Errorf("%s ledger lost the local pending review: %+v", name, rv)
		}
	}
	// A second sync merges nothing again: the kept review is already there.
	again, err := SyncLedger(ctx, st, path, "laptop-b", "laptop-a", false, io.Discard)
	if err != nil || len(again.Reviews) != 2 {
		t.Fatalf("a second sync duplicated or dropped reviews: %d, %v", len(again.Reviews), err)
	}
}

// A forced reduce and apply on a non-reducer host publishes a change but
// can't write the store ledger, so its reviews go to a per-host store object
// the reducer folds into the ledger at its next sync. Done-when: host B's
// publish writes the object, the reducer's next sync sees the review as
// pending (and a matching proposal is held back instead of re-proposed),
// a second merge adds nothing, and a verdict refresh decides it.
func TestHostReviewsReachTheReducer(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	published := &Ledger{Rules: []Rule{storeRule()}, LastRule: 1}
	if _, err := PutStoreLedger(ctx, st, published, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	// Host B's forced run recorded a pending review the store ledger lacks.
	pending := Review{Kind: "add", Title: "Prefer table tests",
		Rule: "Prefer table tests over copies.", RuleHash: ruleHash("Prefer table tests over copies."),
		Via: "pr", Where: "https://example.com/you/dotfiles/pull/2", Repo: "/home/you/dotfiles",
		Branch: "learn-2026-11-04", Verdict: review.Pending, Proposed: "2026-11-04"}
	localB := &Ledger{Rules: []Rule{storeRule()}, Reviews: []Review{pending}, LastRule: 1}
	when := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	key, err := PutHostReviews(ctx, st, "laptop-b", localB, when)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "analysis/reviews/laptop-b/") {
		t.Fatalf("host review object key = %q, want it under analysis/reviews/laptop-b/", key)
	}
	// The same reviews again land on the same object: the key hashes the
	// review keys, so nothing piles up.
	again, err := PutHostReviews(ctx, st, "laptop-b", localB, when.Add(time.Hour))
	if err != nil || again != key {
		t.Fatalf("second put = %q, %v; want the same object %q", again, err, key)
	}
	if keys, _ := st.List(ctx, "analysis/reviews/"); len(keys) != 1 {
		t.Fatalf("host review objects = %v, want one", keys)
	}

	// The reducer's next sync folds the review in, as pending.
	pathA := filepath.Join(t.TempDir(), "ledger.json")
	if err := published.Save(pathA); err != nil {
		t.Fatal(err)
	}
	var note bytes.Buffer
	got, err := SyncLedger(ctx, st, pathA, "laptop-a", "laptop-a", true, &note)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Reviews) != 1 || got.Reviews[0].Title != pending.Title || got.Reviews[0].Verdict != review.Pending {
		t.Fatalf("reducer's synced reviews = %+v, want host B's pending one", got.Reviews)
	}
	if !bytes.Contains(note.Bytes(), []byte("folded 1 review(s)")) {
		t.Errorf("no note about the fold: %q", note.String())
	}
	onDisk, err := LoadLedger(pathA)
	if err != nil || len(onDisk.Reviews) != 1 {
		t.Fatalf("the reducer's local file didn't keep the folded review: %+v, %v", onDisk, err)
	}
	// The pending review holds a matching proposal back, so the reduce's
	// output isn't applied twice while the owner decides.
	a := &Applier{Ledger: got}
	if why := a.reviewed(Proposal{Kind: "add", Title: pending.Title}); why == "" {
		t.Error("a proposal matching the folded pending review wasn't held back")
	}

	// A second sync, and a sync after the merged ledger is published, add
	// nothing: the fold is idempotent by the review's key.
	if _, err := PutStoreLedger(ctx, st, got, time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		resync, err := SyncLedger(ctx, st, pathA, "laptop-a", "laptop-a", true, io.Discard)
		if err != nil || len(resync.Reviews) != 1 {
			t.Fatalf("sync %d duplicated or dropped the review: %d, %v", i, len(resync.Reviews), err)
		}
	}

	// The reducer tracks the folded review's verdict like any other.
	pub := &review.Fake{Verdicts: map[string]review.Verdict{pending.Where: review.Closed}, Decided: "2026-11-06"}
	n, err := got.RefreshReviews(ctx, func(string) (review.Publisher, error) { return pub, nil },
		time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC))
	if err != nil || n != 1 {
		t.Fatalf("refresh decided %d review(s), %v; want 1", n, err)
	}
	if got.Reviews[0].Verdict != review.Closed || got.Reviews[0].Decided != "2026-11-06" {
		t.Fatalf("folded review after refresh = %+v, want closed on 2026-11-06", got.Reviews[0])
	}
}

// The reducer folds host review objects in even before any store ledger
// version exists, and other hosts never fold them: only the reducer carries
// them onto the shared ledger.
func TestHostReviewsFoldOnlyOnTheReducer(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	pending := Review{Kind: "add", Title: "Prefer table tests", Via: "pr",
		Where: "https://example.com/you/dotfiles/pull/2", Repo: "/home/you/dotfiles",
		Verdict: review.Pending, Proposed: "2026-11-04"}
	localB := &Ledger{Reviews: []Review{pending}}
	if key, err := PutHostReviews(ctx, st, "laptop-b", localB, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)); err != nil || key == "" {
		t.Fatalf("put with no store ledger = %q, %v; want the object written", key, err)
	}
	pathA := filepath.Join(t.TempDir(), "ledger.json")
	got, err := SyncLedger(ctx, st, pathA, "laptop-a", "laptop-a", true, io.Discard)
	if err != nil || len(got.Reviews) != 1 {
		t.Fatalf("reducer with no store version folded %d review(s), %v; want 1", len(got.Reviews), err)
	}
	pathC := filepath.Join(t.TempDir(), "ledger.json")
	other, err := SyncLedger(ctx, st, pathC, "laptop-c", "laptop-a", false, io.Discard)
	if err != nil || len(other.Reviews) != 0 {
		t.Fatalf("a non-reducer host folded %d review(s), %v; want 0", len(other.Reviews), err)
	}
}

// A host review object the ledger already folded costs no Get on a later
// sync: the ledger remembers the objects it read, so a map, reduce, or
// apply pays one read for the store ledger and nothing per settled object.
// Once the published store ledger carries an object's reviews, the reducer
// deletes the object.
func TestMergedHostReviewObjectsAreReadOnceThenPruned(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	published := &Ledger{Rules: []Rule{storeRule()}, LastRule: 1}
	if _, err := PutStoreLedger(ctx, st, published, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	pending := Review{Kind: "add", Title: "Prefer table tests", Via: "pr",
		Where: "https://example.com/you/dotfiles/pull/2", Repo: "/home/you/dotfiles",
		Verdict: review.Pending, Proposed: "2026-11-04"}
	localB := &Ledger{Rules: []Rule{storeRule()}, Reviews: []Review{pending}, LastRule: 1}
	if _, err := PutHostReviews(ctx, st, "laptop-b", localB, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	pathA := filepath.Join(t.TempDir(), "ledger.json")
	if err := published.Save(pathA); err != nil {
		t.Fatal(err)
	}
	got, err := SyncLedger(ctx, st, pathA, "laptop-a", "laptop-a", true, io.Discard)
	if err != nil || len(got.Reviews) != 1 {
		t.Fatalf("first sync folded %d review(s), %v; want 1", len(got.Reviews), err)
	}

	// The second sync reads the store's newest ledger version and nothing
	// else: the folded object is skipped without a Get.
	cs := &countingStore{Store: st}
	got, err = SyncLedger(ctx, cs, pathA, "laptop-a", "laptop-a", true, io.Discard)
	if err != nil || len(got.Reviews) != 1 {
		t.Fatalf("second sync: %d review(s), %v; want 1", len(got.Reviews), err)
	}
	if cs.gets != 1 {
		t.Errorf("second sync read %d object(s), want 1 (the store ledger alone)", cs.gets)
	}
	// Not pruned yet: the store's newest ledger version lacks the review.
	if keys, _ := st.List(ctx, storeReviewsPrefix); len(keys) != 1 {
		t.Errorf("the object was pruned before its reviews were published: %v", keys)
	}

	// Publishing the folded ledger lets the next sync delete the object.
	if _, err := PutStoreLedger(ctx, st, got, time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	cs = &countingStore{Store: st}
	got, err = SyncLedger(ctx, cs, pathA, "laptop-a", "laptop-a", true, io.Discard)
	if err != nil || len(got.Reviews) != 1 {
		t.Fatalf("sync after publish: %d review(s), %v; want 1", len(got.Reviews), err)
	}
	if cs.gets != 1 {
		t.Errorf("sync after publish read %d object(s), want 1 (the store ledger alone)", cs.gets)
	}
	if keys, _ := st.List(ctx, storeReviewsPrefix); len(keys) != 0 {
		t.Errorf("host review objects published long ago = %v, want them deleted", keys)
	}
	if len(got.Folded) != 0 {
		t.Errorf("the ledger still tracks pruned objects: %v", got.Folded)
	}
}

// flakyStore fails every Get under a prefix, like a transient store error.
type flakyStore struct {
	store.Store
	prefix string
}

func (f *flakyStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if strings.HasPrefix(key, f.prefix) {
		return nil, errors.New("transient store error")
	}
	return f.Store.Get(ctx, key)
}

// A transient error reading a host review object degrades on a lenient
// sync — the map path, which only shows rules to the model — to a warning,
// and the sync keeps what it read. A strict sync — the reduce and apply
// paths, which hold a proposal back when a pending review matches it —
// still fails, since a review missed there re-proposes an open change.
func TestHostReviewReadErrorDegradesUnlessStrict(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	published := &Ledger{Rules: []Rule{storeRule()}, LastRule: 1}
	if _, err := PutStoreLedger(ctx, st, published, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	pending := Review{Kind: "add", Title: "Prefer table tests", Via: "pr",
		Where: "https://example.com/you/dotfiles/pull/2", Repo: "/home/you/dotfiles",
		Verdict: review.Pending, Proposed: "2026-11-04"}
	localB := &Ledger{Rules: []Rule{storeRule()}, Reviews: []Review{pending}, LastRule: 1}
	if _, err := PutHostReviews(ctx, st, "laptop-b", localB, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	flaky := &flakyStore{Store: st, prefix: storeReviewsPrefix}

	pathA := filepath.Join(t.TempDir(), "ledger.json")
	if err := published.Save(pathA); err != nil {
		t.Fatal(err)
	}
	var note bytes.Buffer
	got, err := SyncLedger(ctx, flaky, pathA, "laptop-a", "laptop-a", false, &note)
	if err != nil {
		t.Fatalf("a lenient sync failed on a host review read: %v", err)
	}
	if len(got.Reviews) != 0 {
		t.Fatalf("a lenient sync invented reviews it couldn't read: %+v", got.Reviews)
	}
	if !bytes.Contains(note.Bytes(), []byte("warning")) {
		t.Errorf("no warning about the skipped host reviews: %q", note.String())
	}

	if _, err := SyncLedger(ctx, flaky, pathA, "laptop-a", "laptop-a", true, io.Discard); err == nil {
		t.Fatal("a strict sync swallowed the host review read error")
	}

	// With the store healthy again, the next sync folds the review in.
	got, err = SyncLedger(ctx, st, pathA, "laptop-a", "laptop-a", true, io.Discard)
	if err != nil || len(got.Reviews) != 1 {
		t.Fatalf("sync after recovery: %d review(s), %v; want 1", len(got.Reviews), err)
	}
}

func TestSyncLedgerWithoutStoreVersionKeepsLocal(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	path := filepath.Join(t.TempDir(), "ledger.json")
	local := &Ledger{Rules: []Rule{storeRule()}, LastRule: 1}
	if err := local.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := SyncLedger(ctx, st, path, "laptop-b", "laptop-a", false, io.Discard)
	if err != nil || len(got.Rules) != 1 || got.Rules[0].ID != "r-0001" {
		t.Fatalf("with no store version the local ledger must stand: %+v, %v", got.Rules, err)
	}
	if keys, _ := st.List(ctx, "ledger/"); len(keys) != 0 {
		t.Fatalf("a sync wrote the store: %v; only the reducer's reduce and apply write it", keys)
	}
}
