package config

import (
	"path/filepath"
	"regexp"
)

// RecurPrefix marks recall's crontab block and prefixes its LaunchAgent labels.
const RecurPrefix = "recall.recur"

// SchedulerName limits recur job names and the prefix: launchd needs a
// label and logs a filename.
var SchedulerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// LedgerPath is the rule ledger under home.
func LedgerPath(home string) string {
	return filepath.Join(home, ".local", "state", "recall", "ledger.json")
}

// CandidatesPath is the candidate ledger under home.
func CandidatesPath(home string) string {
	return filepath.Join(home, ".local", "state", "recall", "learn", "candidates.json")
}
