package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The owner-evidence check is on in production: a proposal that cites only
// tool evidence is held back, and the removed key can't turn the check off.
func TestOwnerEvidenceCheckAlwaysOn(t *testing.T) {
	for name, toml := range map[string]string{
		"default":    "[learn]\nreview = \"branch\"\n\n[hooks]\nmap = true\nlearn_every = 7\n",
		"key is off": "[learn]\nreview = \"branch\"\nrequire_owner_evidence = false\n\n[hooks]\nmap = true\nlearn_every = 7\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newHookFixtureWith(t, toml)
			f.transcript(t, "sess-old", f.clk.T.AddDate(0, 0, -5))
			hookWork(f.env)
			proposed := false
			for _, in := range f.runner.calls {
				if strings.Contains(in, "# CANDIDATES") && strings.Contains(in, "Candidate[") {
					proposed = true
				}
			}
			if !proposed {
				t.Fatal("the reduce never had a candidate to propose from")
			}
			if got := f.pending(t); got != 0 {
				t.Errorf("pending reviews = %d, want 0: a proposal with no owner-typed evidence got through", got)
			}
			if !held(t, filepath.Join(f.home, ".local", "state", "recall"), "cites no observation the owner typed") {
				t.Error("no output says the proposal was held back for owner evidence")
			}
		})
	}
}

// held reports whether any file under dir contains text.
func held(t *testing.T, dir, text string) bool {
	t.Helper()
	found := false
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(b), text) {
				found = true
			}
		}
		return nil
	})
	return found
}
