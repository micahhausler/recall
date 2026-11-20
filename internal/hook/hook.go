// Package hook is what the harness's own hooks need from recall, so the
// learning loop runs with no scheduler: a session end that starts the
// loop's work in the background. Nothing runs at session start, so recall
// can never delay a session.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

// GuardEnv is set in the environment of every harness process recall
// starts. A recall hook that sees it does nothing: the session ending is
// one of recall's own model calls, and working on it would start another.
const GuardEnv = "RECALL_HOOK"

// Guard is GuardEnv as the environment entry every harness run recall
// starts carries; model runners and harness inventories append it.
const Guard = GuardEnv + "=1"

// Guarded reports whether this process was started, directly or not, by a
// harness run recall itself made.
func Guarded() bool { return os.Getenv(GuardEnv) != "" }

// Input is the JSON a harness hands a hook on stdin; only the fields
// recall reads.
type Input struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	Event          string `json:"hook_event_name"`
	// Reason is why a session ended.
	Reason string `json:"reason"`
}

// ReadInput reads a hook's stdin. Input that isn't the expected JSON is an
// empty Input, since a hook must never fail a session over its own input.
func ReadInput(r io.Reader) Input {
	var in Input
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err == nil {
		_ = json.Unmarshal(data, &in)
	}
	return in
}

// Due reports whether a periodic step last ran at least every ago, by its
// stamp file's time. A step that never ran is due.
func Due(stamp string, every time.Duration, now time.Time) bool {
	st, err := os.Stat(stamp)
	return err != nil || now.Sub(st.ModTime()) >= every
}

// Touch marks a periodic step as run now.
func Touch(stamp string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		return err
	}
	return os.Chtimes(stamp, now, now)
}

// Since returns the moment a step works from, recording start as that
// moment the first time it is asked. Later calls return what was recorded,
// so the step carries on from where it began, whatever start they pass.
func Since(stamp string, start time.Time) time.Time {
	if data, err := os.ReadFile(stamp); err == nil {
		if t, err := time.Parse(time.RFC3339, string(data)); err == nil {
			return t
		}
	}
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err == nil {
		_ = os.WriteFile(stamp, []byte(start.UTC().Format(time.RFC3339)), 0o644)
	}
	return start
}

// Lock takes a lock directory so one background worker runs at a time. ok
// is false when another holds it. A lock older than stale was left by a
// worker that died, and is taken over. release gives the lock up only if
// this worker still holds it: a worker that outlived stale and was taken
// over must not free the lock of the worker that replaced it.
func Lock(dir string, stale time.Duration, now time.Time) (release func(), ok bool) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, false
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		st, serr := os.Stat(dir)
		if serr != nil || now.Sub(st.ModTime()) < stale {
			return nil, false
		}
		// Older than stale: a dead worker's lock. Move it aside and make a
		// fresh one; only the worker whose Mkdir lands gets the lock, so
		// two that both saw it stale can't both run.
		aside := fmt.Sprintf("%s.stale-%d-%d", dir, os.Getpid(), rand.Int64())
		if os.Rename(dir, aside) == nil {
			_ = os.RemoveAll(aside)
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return nil, false
		}
	}
	token := fmt.Sprintf("%d-%d", os.Getpid(), now.UnixNano())
	owner := filepath.Join(dir, "owner")
	if err := os.WriteFile(owner, []byte(token), 0o644); err != nil {
		// Remove the directory this worker just made, or it would sit
		// there as an ownerless held lock until the stale window passed.
		_ = os.RemoveAll(dir)
		return nil, false
	}
	// The lock's age is counted from the injected clock.
	if err := os.Chtimes(dir, now, now); err != nil {
		_ = os.RemoveAll(dir)
		return nil, false
	}
	return func() {
		if held, err := os.ReadFile(owner); err == nil && string(held) == token {
			os.Remove(owner)
			os.Remove(dir)
		}
	}, true
}
