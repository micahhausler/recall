package main

import (
	"testing"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/model"
)

// Every harness in config's table has an adapter that reads its
// transcripts and a runner the loop calls it through, registered under the
// same name, so a harness added to the table isn't half there.
func TestKnownHarnessesAreWired(t *testing.T) {
	for _, name := range config.KnownHarnesses {
		h, err := harness.Open(name, config.HarnessSettings{Source: config.DefaultSource(name, "/home/you"), Enabled: true})
		if err != nil {
			t.Errorf("%s: no harness adapter: %v", name, err)
		} else if h.Name() != name {
			t.Errorf("%s: its adapter calls itself %q", name, h.Name())
		}
		if _, err := model.Open(name); err != nil {
			t.Errorf("%s: no model runner: %v", name, err)
		}
	}
}
