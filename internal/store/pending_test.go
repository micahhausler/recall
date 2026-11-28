package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestPending(t *testing.T, remote Store) (*Pending, string, string) {
	t.Helper()
	root := t.TempDir()
	dir, marks := filepath.Join(root, "pending"), filepath.Join(root, "uploaded")
	p, err := NewPending(dir, marks, remote)
	if err != nil {
		t.Fatal(err)
	}
	return p, dir, marks
}

func TestPendingContract(t *testing.T) {
	p, _, _ := newTestPending(t, NewMem())
	contract(t, p)
	q, _, _ := newTestPending(t, NewMem())
	contractUnder(t, q, AnalysisPrefix)
}

// downStore fails every call, like a bucket that is missing or unreachable.
type downStore struct{}

var errDown = errors.New("operation error S3: NoSuchBucket")

func (downStore) PutIfAbsent(context.Context, string, io.Reader) error { return errDown }
func (downStore) Put(context.Context, string, io.Reader) error         { return errDown }
func (downStore) Get(context.Context, string) (io.ReadCloser, error)   { return nil, errDown }
func (downStore) List(context.Context, string) ([]string, error)       { return nil, errDown }
func (downStore) Head(context.Context, string) (Info, error)           { return Info{}, errDown }
func (downStore) Delete(context.Context, string) error                 { return errDown }

func get(t *testing.T, s Store, key string) string {
	t.Helper()
	r, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer r.Close()
	data, _ := io.ReadAll(r)
	return string(data)
}

func TestPendingWritesAnalysisLocallyAndOtherKeysRemotely(t *testing.T) {
	ctx := context.Background()
	remote := NewMem()
	p, dir, _ := newTestPending(t, remote)
	if err := p.PutIfAbsent(ctx, "analysis/claude/s1-abc.json", strings.NewReader("obs")); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"learn/reducer", "analysis/reviews/laptop-a/r.json"} {
		if err := p.PutIfAbsent(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	if keys, _ := remote.List(ctx, ""); strings.Join(keys, ",") != "analysis/reviews/laptop-a/r.json,learn/reducer" {
		t.Errorf("remote = %v, want only the reviews and learn/reducer", keys)
	}
	data, err := os.ReadFile(filepath.Join(dir, "analysis", "claude", "s1-abc.json"))
	if err != nil || string(data) != "obs" {
		t.Errorf("local file = %q, %v; want the key mirrored under the directory", data, err)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("pending dir mode = %v, %v; want 0700", fi.Mode().Perm(), err)
	}
	if n, err := p.Waiting(); err != nil || n != 1 {
		t.Errorf("waiting = %d, %v; want 1", n, err)
	}
}

func TestPendingReadsRemoteAndMergesList(t *testing.T) {
	ctx := context.Background()
	remote := NewMem()
	if err := remote.PutIfAbsent(ctx, "analysis/claude/old.json", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	if err := remote.PutIfAbsent(ctx, "analysis/claude/both.json", strings.NewReader("both")); err != nil {
		t.Fatal(err)
	}
	p, _, _ := newTestPending(t, remote)
	if err := p.PutIfAbsent(ctx, "analysis/claude/old.json", strings.NewReader("again")); !errors.Is(err, ErrExists) {
		t.Errorf("put over a remote key = %v, want ErrExists", err)
	}
	if err := p.local.PutIfAbsent(ctx, "analysis/claude/both.json", strings.NewReader("both")); err != nil {
		t.Fatal(err)
	}
	if err := p.PutIfAbsent(ctx, "analysis/claude/new.json", strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	if got := get(t, p, "analysis/claude/old.json"); got != "old" {
		t.Errorf("get remote-only = %q", got)
	}
	keys, err := p.List(ctx, "analysis/claude/")
	want := []string{"analysis/claude/both.json", "analysis/claude/new.json", "analysis/claude/old.json"}
	if err != nil || strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("list = %v, %v; want each key once: %v", keys, err, want)
	}
}

func TestPendingUploadCopiesWhatTheRemoteLacks(t *testing.T) {
	ctx := context.Background()
	remote := NewMem()
	if err := remote.PutIfAbsent(ctx, "analysis/claude/a.json", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	p, _, _ := newTestPending(t, remote)
	for _, k := range []string{"analysis/claude/a.json", "analysis/claude/b.json", "analysis/hits/claude/c.json"} {
		if err := p.local.PutIfAbsent(ctx, k, strings.NewReader(k)); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := p.Waiting(); n != 3 {
		t.Fatalf("waiting = %d, want 3", n)
	}
	copied, err := p.Upload(ctx, io.Discard)
	if err != nil || copied != 2 {
		t.Errorf("upload = %d, %v; want 2 copied, the existing one counted done", copied, err)
	}
	if got := get(t, remote, "analysis/claude/a.json"); got != "a" {
		t.Errorf("upload replaced a remote object: %q", got)
	}
	for _, k := range []string{"analysis/claude/b.json", "analysis/hits/claude/c.json"} {
		if got := get(t, remote, k); got != k {
			t.Errorf("remote %s = %q, want the local bytes", k, got)
		}
	}
	if n, _ := p.Waiting(); n != 0 {
		t.Errorf("waiting after upload = %d, want 0", n)
	}
	if copied, err := p.Upload(ctx, io.Discard); err != nil || copied != 0 {
		t.Errorf("second upload = %d, %v; want nothing to do", copied, err)
	}
}

func TestPendingPassesOverADownRemote(t *testing.T) {
	ctx := context.Background()
	p, dir, marks := newTestPending(t, downStore{})
	if _, err := p.Get(ctx, "analysis/claude/s1.json"); !errors.Is(err, ErrNotExist) {
		t.Errorf("get with the remote down = %v, want ErrNotExist", err)
	}
	if _, err := p.Head(ctx, "analysis/claude/s1.json"); !errors.Is(err, ErrNotExist) {
		t.Errorf("head with the remote down = %v, want ErrNotExist", err)
	}
	if err := p.PutIfAbsent(ctx, "analysis/claude/s1.json", strings.NewReader("obs")); err != nil {
		t.Errorf("put with the remote down = %v", err)
	}
	if keys, err := p.List(ctx, "analysis/claude/"); err != nil || len(keys) != 1 {
		t.Errorf("list with the remote down = %v, %v; want the local key", keys, err)
	}
	if _, err := p.Get(ctx, "learn/reducer"); !errors.Is(err, errDown) {
		t.Errorf("a non-analysis get = %v, want the remote's error", err)
	}
	if !errors.Is(p.RemoteErr(), errDown) {
		t.Errorf("RemoteErr = %v, want the remote's error", p.RemoteErr())
	}
	if _, err := p.Upload(ctx, io.Discard); !errors.Is(err, errDown) {
		t.Errorf("upload to a down remote = %v", err)
	}
	if n, err := Waiting(dir, marks); err != nil || n != 1 {
		t.Errorf("waiting = %d, %v; want 1 still waiting", n, err)
	}
}

func TestWaitingOnAMissingDirectoryIsZero(t *testing.T) {
	root := t.TempDir()
	if n, err := Waiting(filepath.Join(root, "pending"), filepath.Join(root, "uploaded")); err != nil || n != 0 {
		t.Errorf("waiting = %d, %v; want 0", n, err)
	}
}

// A writer killed between its claim and its rename leaves an empty file;
// it is no object, a later write takes it over, and nothing uploads it.
func TestPendingEmptyClaimIsNoObject(t *testing.T) {
	ctx := context.Background()
	remote := NewMem()
	p, dir, _ := newTestPending(t, remote)
	key := "analysis/claude/s1-abc.json"
	path := filepath.Join(dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(ctx, key); !errors.Is(err, ErrNotExist) {
		t.Errorf("get of an empty claim = %v, want ErrNotExist", err)
	}
	if _, err := p.Head(ctx, key); !errors.Is(err, ErrNotExist) {
		t.Errorf("head of an empty claim = %v, want ErrNotExist", err)
	}
	if n, _ := p.Waiting(); n != 0 {
		t.Errorf("waiting counts an empty claim: %d", n)
	}
	if copied, err := p.Upload(ctx, io.Discard); err != nil || copied != 0 {
		t.Errorf("upload of an empty claim = %d, %v; want nothing", copied, err)
	}
	if keys, _ := remote.List(ctx, ""); len(keys) != 0 {
		t.Errorf("an empty claim reached the remote: %v", keys)
	}
	// Once the claim is old, the next write takes it over.
	old := time.Now().Add(-2 * claimGrace)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := p.PutIfAbsent(ctx, key, strings.NewReader("obs")); err != nil {
		t.Fatalf("put over a dead claim = %v", err)
	}
	if got := get(t, p, key); got != "obs" {
		t.Errorf("get after takeover = %q", got)
	}
}

func TestPendingKeepsTheFilesPrivate(t *testing.T) {
	ctx := context.Background()
	p, dir, _ := newTestPending(t, NewMem())
	key := "analysis/hits/claude/s1-abc.json"
	if err := p.PutIfAbsent(ctx, key, strings.NewReader("obs")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key)))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	for _, sub := range []string{"analysis", "analysis/hits", "analysis/hits/claude"} {
		fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(sub)))
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v, %v; want 0700", sub, fi.Mode().Perm(), err)
		}
	}
}

// A key the remote takes, dots included, is held locally too.
func TestPendingTakesDottedKeys(t *testing.T) {
	ctx := context.Background()
	remote := NewMem()
	p, _, _ := newTestPending(t, remote)
	key := "analysis/kiro/a..b-abc.json"
	if err := p.PutIfAbsent(ctx, key, strings.NewReader("obs")); err != nil {
		t.Fatalf("put %s = %v", key, err)
	}
	if got := get(t, p, key); got != "obs" {
		t.Errorf("get = %q", got)
	}
	if keys, _ := remote.List(ctx, ""); len(keys) != 0 {
		t.Errorf("a dotted key went to the remote: %v", keys)
	}
	if _, err := p.Get(ctx, "analysis/kiro/../../escape"); err == nil {
		t.Error("an escaping key read without an error")
	}
}

// countingStore counts calls and fails each one.
type countingStore struct {
	downStore
	calls int
}

func (c *countingStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.calls++
	return nil, errDown
}
func (c *countingStore) Head(ctx context.Context, key string) (Info, error) {
	c.calls++
	return Info{}, errDown
}
func (c *countingStore) List(ctx context.Context, prefix string) ([]string, error) {
	c.calls++
	return nil, errDown
}

func TestPendingStopsAskingADownRemote(t *testing.T) {
	ctx := context.Background()
	remote := &countingStore{}
	p, _, _ := newTestPending(t, remote)
	for range 5 {
		p.Get(ctx, "analysis/claude/s1.json")
		p.Head(ctx, "analysis/claude/s1.json")
		p.List(ctx, "analysis/claude/")
		if err := p.PutIfAbsent(ctx, "analysis/claude/s2.json", strings.NewReader("x")); err != nil && !errors.Is(err, ErrExists) {
			t.Fatal(err)
		}
	}
	if remote.calls != 1 {
		t.Errorf("remote calls = %d, want 1 before the store stops asking", remote.calls)
	}
}

// cancelStore answers every call with the context's error.
type cancelStore struct{ downStore }

func (cancelStore) Get(ctx context.Context, key string) (io.ReadCloser, error) { return nil, ctx.Err() }
func (cancelStore) Head(ctx context.Context, key string) (Info, error)         { return Info{}, ctx.Err() }

func TestPendingReturnsACanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, _, _ := newTestPending(t, cancelStore{})
	if _, err := p.Get(ctx, "analysis/claude/s1.json"); !errors.Is(err, context.Canceled) {
		t.Errorf("get = %v, want context.Canceled", err)
	}
	if _, err := p.Head(ctx, "analysis/claude/s1.json"); !errors.Is(err, context.Canceled) {
		t.Errorf("head = %v, want context.Canceled", err)
	}
	if p.RemoteErr() != nil {
		t.Errorf("a canceled call counted as the remote failing: %v", p.RemoteErr())
	}
}

func TestPendingUploadWarnsOnADifferentRemoteBody(t *testing.T) {
	ctx := context.Background()
	remote := NewMem()
	if err := remote.PutIfAbsent(ctx, "analysis/claude/a.json", strings.NewReader("remote")); err != nil {
		t.Fatal(err)
	}
	p, _, _ := newTestPending(t, remote)
	if err := p.local.PutIfAbsent(ctx, "analysis/claude/a.json", strings.NewReader("local secret")); err != nil {
		t.Fatal(err)
	}
	var warn strings.Builder
	if _, err := p.Upload(ctx, &warn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "analysis/claude/a.json") || strings.Contains(warn.String(), "secret") {
		t.Errorf("warning = %q, want the key named and no content", warn.String())
	}
	if n, _ := p.Waiting(); n != 0 {
		t.Errorf("waiting after a mismatch = %d, want it marked", n)
	}
}
