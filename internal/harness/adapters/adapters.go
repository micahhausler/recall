// Package adapters opens a harness by its config name. It lists every adapter
// recall ships, so a caller that opens one can't miss an adapter's
// registration.
package adapters

import (
	"fmt"
	"sort"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/harness/claude"
	"github.com/micahhausler/recall/internal/harness/kiro"
	"github.com/micahhausler/recall/internal/harness/stored"
)

var known = map[string]harness.Factory{
	config.HarnessClaude: claude.New,
	config.HarnessKiro:   kiro.New,
}

// Open opens one named harness. A name recall doesn't know opens the
// adapter its settings' Format names, under that name; the events format
// is recall's own.
func Open(name string, s config.HarnessSettings) (harness.Harness, error) {
	if f, ok := known[name]; ok {
		return f(s)
	}
	if s.Format == "" {
		return nil, fmt.Errorf("unknown harness %q; set harness.%s.format to the known harness whose transcript format it shares", name, name)
	}
	if s.Format == config.FormatEvents {
		return stored.New(name, s), nil
	}
	f, ok := known[s.Format]
	if !ok {
		return nil, fmt.Errorf("harness %q: unknown format %q", name, s.Format)
	}
	h, err := f(s)
	if err != nil {
		return nil, err
	}
	return &formatted{Harness: h, name: name}, nil
}

// Enabled opens every enabled harness in the settings, sorted by name.
func Enabled(s *config.Settings) ([]harness.Harness, error) {
	names := make([]string, 0, len(s.Harnesses))
	for n := range s.Harnesses {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []harness.Harness
	for _, n := range names {
		h, err := Open(n, s.Harnesses[n])
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}
