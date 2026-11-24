// Package v1 is the recall.import/v1 contract (docs/import.md): the
// request recall hands an import plugin in its environment and the answer
// it reads back from the plugin's stdout. The session files the plugin
// writes are recall.events/v1 (api/events/v1). It imports only the
// standard library.
//
// The contract grows by adding optional fields to Spec and Status. Both
// sides ignore fields they don't know, so decode with encoding/json's
// default, never DisallowUnknownFields.
package v1

import (
	"regexp"
	"strings"
)

// The apiVersion and kind every object carries, in both directions.
// Either side refuses anything else.
const (
	APIVersion = "recall.import/v1"
	Kind       = "Import"
)

// InfoEnv is the environment variable recall sets for the plugin to the
// request, an Import as JSON with Spec set. The plugin's stdin is empty.
const InfoEnv = "RECALL_IMPORT_INFO"

// Import is the contract's one object: recall fills in Spec, and the
// plugin answers with Status.
type Import struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// Spec is what recall asks for. A plugin may echo it, and recall
	// ignores the echo.
	Spec *Spec `json:"spec,omitempty"`
	// Status is nil in what recall sends. The plugin sets it.
	Status *Status `json:"status,omitempty"`
}

// Spec is what recall asks for.
type Spec struct {
	// Name is the harness name, so one plugin can serve two harnesses.
	Name string `json:"name"`
	// OutputDir is an empty directory, mode 0700, for this run only. The
	// plugin writes one <anything>.jsonl per session directly in it.
	OutputDir string `json:"outputDir"`
	// Since is RFC 3339: return every session that changed at or after
	// it. Empty asks for everything.
	Since string `json:"since"`
	// Limits are what recall enforces on the output.
	Limits Limits `json:"limits"`
}

// Limits bound one run's output. A session file over SessionBytes, or
// with a line over LineBytes, is rejected on its own; more than Sessions
// files or TotalBytes in all fails the run.
type Limits struct {
	Sessions     int   `json:"sessions"`
	SessionBytes int64 `json:"sessionBytes"`
	LineBytes    int64 `json:"lineBytes"`
	TotalBytes   int64 `json:"totalBytes"`
}

// DefaultLimits are the limits recall sends.
var DefaultLimits = Limits{Sessions: 10000, SessionBytes: 64 << 20, LineBytes: 4 << 20, TotalBytes: 1 << 30}

// Status is what the plugin reports.
type Status struct {
	// Sessions is how many session files the plugin wrote. Required.
	Sessions int `json:"sessions"`
	// Filtered is how many sessions the plugin left out on purpose, such
	// as ones a local harness already records.
	Filtered int `json:"filtered,omitempty"`
	// Partial is true when the plugin couldn't fetch everything it was
	// asked for; recall stores what arrived and asks again next run.
	Partial bool `json:"partial,omitempty"`
}

// sessionIDRE is the shape of a session id: no slash, no "..", no colon.
var sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidSessionID reports whether id can name a stored session.
func ValidSessionID(id string) bool {
	return sessionIDRE.MatchString(id) && !strings.Contains(id, "..")
}
