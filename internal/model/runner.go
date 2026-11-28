// Package model runs prompts through a harness CLI in headless mode (for
// example `claude -p` or `kiro-cli chat --no-interactive`), never an API
// client, so existing auth, billing, and model selection work unchanged.
// Each harness's runner is a sub-package, and package runners opens one by
// its learn.runner name.
package model

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/hook"
)

// Usage is what one call cost. The harness CLI reports it, so there's no
// pricing table to keep current here.
type Usage struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	// Credits is what a harness that bills in credits, like Kiro, reports
	// instead of tokens and dollars.
	Credits float64
	// ModelID is the model the harness resolved the call to, such as a full
	// model ID for an alias; empty when the harness doesn't report it.
	ModelID string
}

// Add accumulates another call's usage.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + o.InputTokens,
		OutputTokens: u.OutputTokens + o.OutputTokens,
		CostUSD:      u.CostUSD + o.CostUSD,
		Credits:      u.Credits + o.Credits,
		ModelID:      cmp.Or(o.ModelID, u.ModelID),
	}
}

// Zero reports whether the usage records nothing.
func (u Usage) Zero() bool { return u == Usage{} }

// String names only the parts the harness reported.
func (u Usage) String() string {
	var parts []string
	if u.InputTokens > 0 || u.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d in, %d out", u.InputTokens, u.OutputTokens))
	}
	if u.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.4f", u.CostUSD))
	}
	if u.Credits > 0 {
		parts = append(parts, fmt.Sprintf("%.2f credits", u.Credits))
	}
	if len(parts) == 0 {
		return "no usage reported"
	}
	return strings.Join(parts, ", ")
}

// ValidEffort refuses a level the runner's CLI doesn't take. Empty is
// valid: it passes no level, and the harness default applies.
func ValidEffort(runner, effort string) error {
	levels := config.Efforts(runner)
	if effort == "" || slices.Contains(levels, effort) {
		return nil
	}
	return fmt.Errorf("%s: unknown effort %q; want one of %s, or empty for the harness default",
		runner, effort, strings.Join(levels, ", "))
}

// Call names what answers a prompt: the model, and the effort it runs at.
type Call struct {
	Model string
	// Effort is one of the runner's config.Efforts, or empty for the
	// harness default.
	Effort string
}

// Runner answers one prompt with one completion.
type Runner interface {
	// Run sends input to the model call names and returns its text and
	// what the call cost.
	Run(ctx context.Context, call Call, input string) (string, Usage, error)
}

// Checker is a Runner or Editor that can refuse a call before making it,
// such as an effort level its CLI doesn't take.
type Checker interface {
	Check(call Call) error
}

// Check refuses a call r's harness can't make, before anything is spent
// on it.
func Check(r any, call Call) error {
	if c, ok := r.(Checker); ok {
		return c.Check(call)
	}
	return nil
}

// ErrNoEditor is runners.OpenEditor's error for a known runner that has no
// editor.
var ErrNoEditor = errors.New("can't confine file edits to the apply worktrees")

// Editor answers one prompt with file tools confined to a set of
// directories. Only the apply step uses one: every other model call reads
// stdin and writes stdout and nothing else.
type Editor interface {
	// Edit sends input to the model call names, lets it read and edit
	// files in dirs (the first is its working directory), and returns its
	// final text and what the call cost.
	Edit(ctx context.Context, call Call, input string, dirs []string) (string, Usage, error)
}

// HookGuard is set in the environment of every harness run recall starts.
// The harness fires its session hooks for a headless run too, and recall's
// own hooks (internal/hook) do nothing when they see it; without it, a
// session-end hook that maps sessions would be started by the map's own
// model calls.
const HookGuard = hook.Guard

// ErrRefusal is a run the model or its safeguards declined; the input, not
// the runner, is the problem, so a smaller input may still go through.
var ErrRefusal = errors.New("refusal")
