package adapters_test

import (
	"os"
	"path/filepath"
	"testing"

	eventsv1 "github.com/micahhausler/recall/api/events/v1"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/harness/adapters"
)

// Every event an adapter emits from its fixtures passes the contract's
// own check.
func TestAdapterEventsCheck(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("..", "*", "testdata", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures")
	}
	for _, path := range fixtures {
		name := filepath.Base(filepath.Dir(filepath.Dir(path)))
		h, err := adapters.Open(name, config.HarnessSettings{Source: t.TempDir(), Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		evs, err := h.ReadSession(f, "s", "laptop-a")
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		for i, e := range evs {
			if err := eventsv1.Check(e); err != nil {
				t.Errorf("%s event %d: %v", path, i+1, err)
			}
		}
	}
}
