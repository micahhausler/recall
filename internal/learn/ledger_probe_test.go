package learn

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/store"
)

// recordingStore drops the newest ledger version between the probe's list
// and its put, as a lifecycle rule might, and records what the put wrote.
type recordingStore struct {
	*store.Mem
	drop string
	put  []byte
}

func (r *recordingStore) PutIfAbsent(ctx context.Context, key string, body io.Reader) error {
	if key == r.drop {
		_ = r.Mem.Delete(ctx, key)
		r.put, _ = io.ReadAll(body)
		return r.Mem.PutIfAbsent(ctx, key, bytes.NewReader(r.put))
	}
	return r.Mem.PutIfAbsent(ctx, key, body)
}

// The write probe carries the newest version's own bytes, so losing the
// race to a deletion leaves an identical copy, never an empty newest
// version every host's next sync would read.
func TestCheckStoreLedgerWriteProbesWithTheVersionsBytes(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMem()
	key := "ledger/20261120T000000.000000000Z.json"
	body := `{"rules":[{"id":"r-0001"}]}`
	if err := mem.PutIfAbsent(ctx, key, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	st := &recordingStore{Mem: mem, drop: key}
	if err := CheckStoreLedgerWrite(ctx, st); err != nil {
		t.Fatal(err)
	}
	if string(st.put) != body {
		t.Errorf("probe wrote %q, want the version's own bytes", st.put)
	}
}
