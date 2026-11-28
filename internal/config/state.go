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

// PendingDir is where observe writes analysis/ keys before `recall sync`
// uploads them.
func PendingDir(home string) string {
	return filepath.Join(home, ".local", "state", "recall", "pending")
}

// UploadedDir holds a marker per PendingDir key already uploaded.
func UploadedDir(home string) string {
	return filepath.Join(home, ".local", "state", "recall", "uploaded")
}

// SyncStatePath is each step's result from the last `recall sync`.
func SyncStatePath(home string) string {
	return filepath.Join(home, ".local", "state", "recall", "sync.json")
}
