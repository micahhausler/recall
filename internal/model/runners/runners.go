// Package runners opens a model runner by its learn.runner name. It lists
// every runner recall ships, so a caller that opens one can't miss a
// runner's registration.
package runners

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/model/claude"
	"github.com/micahhausler/recall/internal/model/kiro"
)

var known = map[string]func() model.Runner{
	config.HarnessClaude: claude.New,
	config.HarnessKiro:   kiro.New,
}

// editors are the runners whose harness can confine file edits to the
// apply step's directories.
var editors = map[string]func() model.Editor{
	config.HarnessClaude: claude.NewEditor,
}

// Open returns the runner listed under name.
func Open(name string) (model.Runner, error) {
	f, ok := known[name]
	if !ok {
		names := slices.Sorted(maps.Keys(known))
		return nil, fmt.Errorf("unknown learn.runner %q; want one of %s", name, strings.Join(names, ", "))
	}
	return f(), nil
}

// OpenEditor returns the editor for the runner named name. A runner whose
// harness can't confine edits to given directories has none; apply then
// gives that runner no tools and writes the files its answer returns,
// rather than edit through an unconfined harness.
func OpenEditor(name string) (model.Editor, error) {
	f, ok := editors[name]
	if !ok {
		if _, ok := known[name]; ok {
			return nil, fmt.Errorf("learn.runner %q %w", name, model.ErrNoEditor)
		}
		return nil, fmt.Errorf("unknown learn.runner %q", name)
	}
	return f(), nil
}
