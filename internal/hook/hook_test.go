package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestReadInput(t *testing.T) {
	in := ReadInput(strings.NewReader(`{"session_id":"s1","transcript_path":"/t/s1.jsonl","cwd":"/w","hook_event_name":"SessionEnd","reason":"clear","extra":1}`))
	if in.SessionID != "s1" || in.TranscriptPath != "/t/s1.jsonl" || in.Reason != "clear" || in.Event != "SessionEnd" {
		t.Errorf("input = %+v", in)
	}
	if got := ReadInput(strings.NewReader("not json")); got != (Input{}) {
		t.Errorf("bad input = %+v, want empty", got)
	}
}

func TestGuarded(t *testing.T) {
	t.Setenv(GuardEnv, "")
	if Guarded() {
		t.Error("guarded with the variable empty")
	}
	t.Setenv(GuardEnv, "1")
	if !Guarded() {
		t.Error("not guarded with the variable set")
	}
}

func TestDueAndLock(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	stamp := filepath.Join(dir, "state", "archive.stamp")
	if !Due(stamp, time.Hour, now) {
		t.Error("a step that never ran isn't due")
	}
	if err := Touch(stamp, now); err != nil {
		t.Fatal(err)
	}
	if Due(stamp, time.Hour, now.Add(59*time.Minute)) || !Due(stamp, time.Hour, now.Add(time.Hour)) {
		t.Error("due should turn on exactly when the interval has passed")
	}

	since := filepath.Join(dir, "state", "map.since")
	if got := Since(since, now); !got.Equal(now) {
		t.Errorf("first asked: since = %v, want now", got)
	}
	if got := Since(since, now.Add(48*time.Hour)); !got.Equal(now) {
		t.Errorf("asked again: since = %v, want the first time", got)
	}

	lock := filepath.Join(dir, "state", "worker.lock")
	release, ok := Lock(lock, time.Hour, now)
	if !ok {
		t.Fatal("couldn't take a free lock")
	}
	if _, ok := Lock(lock, time.Hour, now.Add(time.Minute)); ok {
		t.Error("took a lock another worker holds")
	}
	release()
	if release, ok = Lock(lock, time.Hour, now); !ok {
		t.Fatal("couldn't take a released lock")
	}
	// A worker that outlives stale is taken over, and when it finally
	// finishes it must not free its successor's lock.
	slow := release
	if _, ok := Lock(lock, time.Hour, now.Add(2*time.Hour)); !ok {
		t.Fatal("a dead worker's lock was never taken over")
	}
	slow()
	if _, ok := Lock(lock, time.Hour, now.Add(2*time.Hour+time.Minute)); ok {
		t.Error("the slow worker's release freed the lock of the worker that took over")
	}
}

// A Lock that made the directory but couldn't write the owner file must
// remove it, or every later worker would see a fresh held lock and refuse
// until the stale window passed.
func TestLockClaimFailureLeavesNoLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions are not enforced for root")
	}
	dir := t.TempDir()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	lock := filepath.Join(dir, "worker.lock")
	// A umask masking every permission bit makes Mkdir create the lock
	// directory with no permissions, so the owner-file write inside fails.
	old := syscall.Umask(0o777)
	_, ok := Lock(lock, time.Hour, now)
	syscall.Umask(old)
	if ok {
		t.Fatal("took a lock whose owner file couldn't be written")
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed claim left the lock directory behind (stat err = %v)", err)
	}
	release, ok := Lock(lock, time.Hour, now.Add(time.Minute))
	if !ok {
		t.Fatal("couldn't take the lock after a failed claim")
	}
	release()
}

// Two workers that both see a stale lock race to take it over; exactly one
// may win, or both would run the loop at once.
func TestLockTakeoverIsAtomic(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for i := range 50 {
		lock := filepath.Join(dir, fmt.Sprintf("worker-%d.lock", i))
		if _, ok := Lock(lock, time.Hour, t0); !ok {
			t.Fatal("couldn't take a free lock")
		}
		// Two hours later the lock is stale, and two workers find it at once.
		later := t0.Add(2 * time.Hour)
		var (
			start sync.WaitGroup
			done  sync.WaitGroup
			wins  atomic.Int32
		)
		start.Add(1)
		for range 2 {
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				if _, ok := Lock(lock, time.Hour, later); ok {
					wins.Add(1)
				}
			}()
		}
		start.Done()
		done.Wait()
		if got := wins.Load(); got != 1 {
			t.Fatalf("round %d: %d workers took over a stale lock, want exactly 1", i, got)
		}
	}
}
