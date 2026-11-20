// Package evidence reads corrections no transcript holds: review comments
// and failed checks on changes a coding agent authored. Sources register
// under the name config uses (learn.evidence), so adding one is a new file,
// not a change to callers.
package evidence

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// SourceReview marks a review comment and SourceCI a failed check. Both
	// are tool-derived evidence: other people's words and machine output,
	// never the owner's own, except a comment the owner wrote (Item.Owner).
	SourceReview = "review"
	SourceCI     = "ci"
)

// Item is one piece of outside evidence: a review comment on, or a failed
// check of, an agent-authored pull request.
type Item struct {
	// Source is SourceReview or SourceCI.
	Source string
	// Repo is the repository as host/path, like github.com/you/example:
	// the same form a checkout's origin has, so the item scopes like a
	// session that ran there.
	Repo string
	// PR names the pull request the item belongs to, like
	// github.com/you/example#12.
	PR string
	// Author is the comment author's login; empty for a failed check.
	Author string
	// Owner reports whether the author is the owner: the login the
	// source's CLI is authenticated as. Only an owner-authored comment may
	// count as owner-typed evidence.
	Owner bool
	// Text is the comment's body, or what failed.
	Text string
	// When is when the comment was written or the check finished.
	When time.Time
}

// Source is one place outside the chat that holds evidence about an
// agent's changes.
type Source interface {
	// Name is the config name the source registered under.
	Name() string
	// Items lists the evidence on agent-authored pull requests updated at
	// or after since.
	Items(ctx context.Context, since time.Time) ([]Item, error)
}

// Options is what a source gets from config.
type Options struct {
	// Repos are the repositories to scan, as host/path slugs: the origins
	// of the learn.repos checkouts, the only repos the loop may read.
	Repos []string
	// Trailer is the commit trailer that marks an agent-authored change
	// (learn.evidence_trailer); empty keeps the source's default.
	Trailer string
}

// Factory opens a source from its options.
type Factory func(o Options) (Source, error)

var registry = map[string]Factory{}

// Register adds a source under its config name.
func Register(name string, f Factory) {
	registry[name] = f
}

// Open opens one named source.
func Open(name string, o Options) (Source, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown evidence source %q: want one of %s", name, strings.Join(Names(), ", "))
	}
	return f(o)
}

// Names lists the registered sources.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Fake is an in-memory source for tests.
type Fake struct {
	// Source is the name Fake answers with; default "fake".
	Source string
	List   []Item
	Err    error
	// Since records each Items call's window.
	Since []time.Time
}

func (f *Fake) Name() string {
	if f.Source == "" {
		return "fake"
	}
	return f.Source
}

func (f *Fake) Items(ctx context.Context, since time.Time) ([]Item, error) {
	f.Since = append(f.Since, since)
	return f.List, f.Err
}
