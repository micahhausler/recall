package learn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/store"
)

// The store holds the authoritative rule ledger for every machine that
// shares it, as immutable versions under this prefix; the newest wins, and
// nothing is overwritten. Only the reducer, the host recorded under
// learn/reducer, writes a version, so only the reducer assigns rule ids.
// Every machine's local ledger file is a cache of the newest version, except
// on the reducer itself, where the local file is authoritative between
// writes.
const storeLedgerPrefix = "ledger/"

// CheckStoreLedgerWrite reports whether this host may write ledger
// versions, without writing one: it repeats the newest version's upload
// with If-None-Match, which a store that allows the write refuses with
// ErrExists, and one that doesn't refuses with its own error, so a reducer
// missing the grant learns before the reduce spends a model call. A store
// with no version yet has nothing to probe, and passes.
func CheckStoreLedgerWrite(ctx context.Context, st store.Store) error {
	keys, err := st.List(ctx, storeLedgerPrefix)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	err = st.PutIfAbsent(ctx, keys[len(keys)-1], strings.NewReader(""))
	if err == nil || errors.Is(err, store.ErrExists) {
		return nil
	}
	return err
}

// LoadStoreLedger returns the newest rule ledger in the store and its key,
// or nil when the store holds none. A torn or foreign object is passed over
// for the version before it.
func LoadStoreLedger(ctx context.Context, st store.Store) (*Ledger, string, error) {
	keys, err := st.List(ctx, storeLedgerPrefix)
	if err != nil {
		return nil, "", err
	}
	for i := len(keys) - 1; i >= 0; i-- {
		if !strings.HasSuffix(keys[i], ".json") {
			continue
		}
		l, ok, err := getJSON[Ledger](ctx, st, keys[i])
		if err != nil {
			return nil, "", err
		}
		if ok {
			return &l, keys[i], nil
		}
	}
	return nil, "", nil
}

// PutStoreLedger writes the ledger to the store as a new version, never
// overwriting an old one. When the newest version already reads the same,
// nothing is written. It returns the key that holds the ledger's content.
func PutStoreLedger(ctx context.Context, st store.Store, l *Ledger, now time.Time) (string, error) {
	data, err := ledgerBytes(l)
	if err != nil {
		return "", err
	}
	newest, newestKey, err := LoadStoreLedger(ctx, st)
	if err != nil {
		return "", err
	}
	if newest != nil {
		if have, err := ledgerBytes(newest); err == nil && bytes.Equal(have, data) {
			return newestKey, nil
		}
	}
	base := storeLedgerPrefix + now.UTC().Format("20060102T150405.000000000") + "Z"
	// A second write in the same instant gets a letter suffix, which sorts
	// after the plain name, so the newest content stays the newest key.
	for i := 0; i <= 'z'-'a'+1; i++ {
		key := base
		if i > 0 {
			key += string(rune('a' + i - 1))
		}
		key += ".json"
		err := st.PutIfAbsent(ctx, key, bytes.NewReader(data))
		if !errors.Is(err, store.ErrExists) {
			return key, err
		}
	}
	return "", fmt.Errorf("%s: too many ledger versions written in one instant", base)
}

// A forced reduce and apply on a host that isn't the reducer publishes a
// change for review but can't write the store ledger, so its reviews go to
// per-host objects under this prefix, the way hosts write their own
// observations and hits. The reducer folds every host's objects into the
// ledger when it syncs. The fold is idempotent by the review's key, so an
// object never resurrects a review the ledger already holds; the ledger's
// Folded set remembers each object read, so none is read twice, and an
// object is deleted once the published store ledger carries everything it
// held.
const storeReviewsPrefix = "analysis/reviews/"

// HostReviews is one host's batch of reviews the store ledger lacked when
// they were recorded.
type HostReviews struct {
	Host    string   `json:"host"`
	Written string   `json:"written"`
	Reviews []Review `json:"reviews"`
}

// PutHostReviews writes the reviews on l that the store's newest ledger
// lacks to a per-host object, for the reducer to fold into the shared ledger
// at its next sync. It returns the key written, or "" when the store ledger
// already holds every review.
func PutHostReviews(ctx context.Context, st store.Store, host string, l *Ledger, now time.Time) (string, error) {
	stored, _, err := LoadStoreLedger(ctx, st)
	if err != nil {
		return "", err
	}
	have := map[string]bool{}
	if stored != nil {
		for _, rv := range stored.Reviews {
			have[reviewKey(rv)] = true
		}
	}
	var missing []Review
	var keys []string
	for _, rv := range l.Reviews {
		if k := reviewKey(rv); !have[k] {
			missing = append(missing, rv)
			keys = append(keys, k)
		}
	}
	if len(missing) == 0 {
		return "", nil
	}
	// The key hashes the review keys, not the time, so writing the same
	// batch again lands on the same object instead of piling up copies.
	key := storeReviewsPrefix + host + "/" + shortHash(keys...) + ".json"
	return key, putJSON(ctx, st, key, HostReviews{Host: host, Written: now.UTC().Format(time.RFC3339), Reviews: missing})
}

// MergeHostReviews folds every host's review objects into l, deduped by the
// review's key, and returns how many reviews it added and whether l changed
// at all. A review already on the ledger, whatever its verdict, wins over
// the stored copy, so running the merge again adds nothing.
//
// An object's store key hashes its batch of review keys, which can't be
// checked against the ledger from the key alone, so l.Folded carries the
// objects already read: a listed object in that set costs no Get. Once
// published — the review keys on the store's newest ledger version — covers
// everything an object carried, the object is deleted and dropped from
// l.Folded, so neither the store nor the set accumulates settled batches.
// Deletion is best-effort: a leftover object is skipped by Folded anyway.
//
// strict says a store error reading the objects fails the merge; otherwise
// it is a warning on warn and the merge keeps what it read. The reduce and
// apply paths must be strict: both hold a proposal back when a pending
// review matches it, so a review missed there re-proposes or re-publishes a
// change that is still open for review. Every other caller (map, rules
// sync, age, reviews) only shows rules or refreshes verdicts, where a
// missed review costs nothing the next sync doesn't repair.
func MergeHostReviews(ctx context.Context, st store.Store, l *Ledger, published map[string]bool, strict bool, warn io.Writer) (added int, changed bool, err error) {
	keys, err := st.List(ctx, storeReviewsPrefix)
	if err != nil {
		if strict {
			return 0, false, err
		}
		fmt.Fprintf(warn, "recall: warning: listing host review objects: %v; synced without them\n", err)
		return 0, false, nil
	}
	seen := map[string]bool{}
	for _, rv := range l.Reviews {
		seen[reviewKey(rv)] = true
	}
	for _, key := range keys {
		carried, folded := l.Folded[key]
		if !folded {
			hr, ok, err := getJSON[HostReviews](ctx, st, key)
			if err != nil {
				if strict {
					return added, changed, err
				}
				fmt.Fprintf(warn, "recall: warning: reading host reviews %s: %v; synced without it\n", key, err)
				continue
			}
			if !ok {
				continue // torn or foreign; never folded, never pruned
			}
			carried = make([]string, 0, len(hr.Reviews))
			for _, rv := range hr.Reviews {
				k := reviewKey(rv)
				carried = append(carried, k)
				if !seen[k] {
					l.Reviews = append(l.Reviews, rv)
					seen[k] = true
					added++
				}
			}
			if l.Folded == nil {
				l.Folded = map[string][]string{}
			}
			l.Folded[key] = carried
			changed = true
		}
		settled := len(carried) > 0
		for _, k := range carried {
			settled = settled && published[k]
		}
		if !settled {
			continue
		}
		if err := st.Delete(ctx, key); err != nil {
			fmt.Fprintf(warn, "recall: warning: pruning folded host reviews %s: %v\n", key, err)
			continue
		}
		delete(l.Folded, key)
		changed = true
	}
	return added, changed, nil
}

// SyncLedger loads the rule ledger for one run, against the store's newest
// version. On the reducer the local file is authoritative, and the store
// copy restores it when the file is gone, with its ids, lineage, and
// reviews; a local file older than the store's newest version (a lower
// LastRule or fewer reviews, both of which only grow) is a restore from a
// backup, replaced by the store's so a reduce can't roll the shared ledger
// back. The reducer then folds in the per-host review objects other hosts'
// forced runs wrote (PutHostReviews), so a change published elsewhere is
// pending here and isn't re-proposed, and its verdict is tracked like any
// review; strict says a store error in that fold fails the sync, which the
// reduce and apply paths need and every other caller degrades to a warning
// (MergeHostReviews says why).
// On every other host the local file is a cache: a missing or
// divergent one is replaced by the store's version, with a one-line note
// when it held anything, so the rule ids this host shows the model are the
// reducer's, and a bullet not yet on the store ledger is not judged until
// the reducer syncs it. A review this host recorded that the store copy
// lacks — a forced reduce and apply here publishes a change but can't write
// the store — is merged into the adopted version too, so this host keeps
// tracking its own published change until the reducer folds its review
// object into the shared ledger. With no store version yet the local ledger
// stands; the reducer's first reduce uploads it as the first version.
func SyncLedger(ctx context.Context, st store.Store, path, host, reducer string, strict bool, warn io.Writer) (*Ledger, error) {
	local, err := LoadLedger(path)
	if err != nil {
		return nil, err
	}
	stored, key, err := LoadStoreLedger(ctx, st)
	if err != nil {
		return nil, err
	}
	empty := len(local.Rules) == 0 && len(local.Reviews) == 0 && local.LastRule == 0
	if host == reducer {
		use := local
		if stored != nil {
			switch {
			case empty:
				use = stored
				fmt.Fprintf(warn, "recall: restored the rule ledger from the store (%s)\n", key)
			// A local file older than the store's newest version is a restore
			// from a backup: publishing it would roll the shared ledger back,
			// so the store version wins. Rule numbers and reviews only grow,
			// so either one lower says the file is older.
			case local.LastRule < stored.LastRule || len(local.Reviews) < len(stored.Reviews):
				use = stored
				fmt.Fprintf(warn, "recall: the local rule ledger (last rule %d, %d review(s)) is older than the store's newest version %s (last rule %d, %d review(s)), like a restore from a backup; adopted the store's\n",
					local.LastRule, len(local.Reviews), key, stored.LastRule, len(stored.Reviews))
			}
		}
		var published map[string]bool
		if stored != nil {
			published = make(map[string]bool, len(stored.Reviews))
			for _, rv := range stored.Reviews {
				published[reviewKey(rv)] = true
			}
		}
		merged, changed, err := MergeHostReviews(ctx, st, use, published, strict, warn)
		if err != nil {
			return nil, err
		}
		if merged > 0 {
			fmt.Fprintf(warn, "recall: folded %d review(s) recorded on other hosts into the rule ledger\n", merged)
		}
		if use != local || merged > 0 || changed {
			if err := use.Save(path); err != nil {
				return nil, err
			}
		}
		return use, nil
	}
	if stored == nil {
		return local, nil
	}
	localData, err := ledgerBytes(local)
	if err != nil {
		return nil, err
	}
	// A review this host recorded that the store copy lacks survives the
	// adoption: a forced reduce and apply here publishes a change but can't
	// write the store, and dropping its review would untrack the published
	// change and re-propose it while still open. It rides this host's cache
	// until the reducer folds the per-host review object the forced run
	// wrote (PutHostReviews) into the shared ledger.
	seen := map[string]bool{}
	for _, rv := range stored.Reviews {
		seen[reviewKey(rv)] = true
	}
	kept := 0
	for _, rv := range local.Reviews {
		if !seen[reviewKey(rv)] {
			stored.Reviews = append(stored.Reviews, rv)
			seen[reviewKey(rv)] = true
			kept++
		}
	}
	storedData, err := ledgerBytes(stored)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(localData, storedData) {
		if !empty {
			fmt.Fprintf(warn, "recall: replaced this host's rule ledger with the store's newest version (%s); only the reducer assigns rule ids\n", key)
		}
		if kept > 0 {
			fmt.Fprintf(warn, "recall: kept %d local review(s) the store ledger lacks; they ride this host's cache until the reducer folds this host's review objects into the shared ledger\n", kept)
		}
		if err := stored.Save(path); err != nil {
			return nil, err
		}
	}
	return stored, nil
}
