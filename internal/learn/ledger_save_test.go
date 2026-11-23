package learn

import (
	"os"
	"path/filepath"
	"testing"
)

// A fresh machine may not have the ledger's directory yet (~/.kiro on a
// Kiro-only one), so Save creates it.
func TestLedgerSaveCreatesItsDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "recall-ledger.json")
	if err := (&Ledger{}).Save(path); err != nil {
		t.Fatalf("Save = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
