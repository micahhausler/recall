package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/learn"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/store"
)

// memRemote is the remote the "memtest" store opens, like an S3 bucket.
var memRemote *store.Mem

// memOpens counts how often the "memtest" store is opened.
var memOpens int

// remoteDown makes the "flaky" store fail every call, like a missing bucket.
var remoteDown bool

var errRemoteDown = errors.New("operation error S3: HeadObject, api error NoSuchBucket")

type flakyStore struct{ store.Store }

func (f flakyStore) PutIfAbsent(ctx context.Context, key string, r io.Reader) error {
	if remoteDown {
		return errRemoteDown
	}
	return f.Store.PutIfAbsent(ctx, key, r)
}
func (f flakyStore) Put(ctx context.Context, key string, r io.Reader) error {
	if remoteDown {
		return errRemoteDown
	}
	return f.Store.Put(ctx, key, r)
}
func (f flakyStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if remoteDown {
		return nil, errRemoteDown
	}
	return f.Store.Get(ctx, key)
}
func (f flakyStore) List(ctx context.Context, prefix string) ([]string, error) {
	if remoteDown {
		return nil, errRemoteDown
	}
	return f.Store.List(ctx, prefix)
}
func (f flakyStore) Head(ctx context.Context, key string) (store.Info, error) {
	if remoteDown {
		return store.Info{}, errRemoteDown
	}
	return f.Store.Head(ctx, key)
}
func (f flakyStore) Delete(ctx context.Context, key string) error {
	if remoteDown {
		return errRemoteDown
	}
	return f.Store.Delete(ctx, key)
}

func init() {
	store.Register("memtest", func(context.Context, config.StoreSettings) (store.Store, error) {
		memOpens++
		return flakyStore{memRemote}, nil
	})
}

// remoteFixture is a hook fixture on a remote store with three sessions.
func remoteFixture(t *testing.T) *hookFixture {
	t.Helper()
	memRemote, remoteDown = store.NewMem(), false
	t.Cleanup(func() { remoteDown = false })
	f := newHookFixture(t)
	f.env.settings.Store.Kind = "memtest"
	for i := range 3 {
		f.transcript(t, fmt.Sprintf("sess-%d", i), f.clk.T.Add(-time.Duration(i+1)*24*time.Hour))
	}
	return f
}

func observeOnce(t *testing.T, f *hookFixture) (calls int, stdout, stderr string) {
	t.Helper()
	before := len(f.runner.calls)
	stderr = captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			if code := learnMap(context.Background(), f.env, f.runner, "30d", 0, false, false, 0, nil); code != 0 {
				t.Errorf("observe = %d", code)
			}
		})
	})
	return len(f.runner.calls) - before, stdout, stderr
}

func waiting(t *testing.T, f *hookFixture) int {
	t.Helper()
	n, err := store.Waiting(config.PendingDir(f.home), config.UploadedDir(f.home))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func upload(t *testing.T, f *hookFixture) {
	t.Helper()
	p, err := store.NewPending(config.PendingDir(f.home), config.UploadedDir(f.home), flakyStore{memRemote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Upload(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestObserveWritesLocallyAndLeavesTheRemoteUntouched(t *testing.T) {
	f := remoteFixture(t)
	calls, out, _ := observeOnce(t, f)
	if calls != 3 {
		t.Fatalf("observe made %d model calls, want 3", calls)
	}
	if keys, _ := memRemote.List(context.Background(), ""); len(keys) != 0 {
		t.Errorf("observe wrote to the remote: %v", keys)
	}
	if n := waiting(t, f); n != 3 {
		t.Errorf("waiting = %d, want 3", n)
	}
	if !strings.Contains(out, "3 objects wait; run `recall sync` to upload them") {
		t.Errorf("observe never said what waits:\n%s", out)
	}
	// Local files mirror the keys learn.Key writes.
	matches, _ := filepath.Glob(filepath.Join(config.PendingDir(f.home), "analysis", "claude", "sess-0-*.json"))
	if len(matches) != 1 {
		t.Fatalf("local copies of sess-0 = %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var so learn.SessionObservations
	if err := json.Unmarshal(data, &so); err != nil {
		t.Fatal(err)
	}
	if want := learn.Key("claude", "sess-0", so.Fingerprint); filepath.Join(config.PendingDir(f.home), filepath.FromSlash(want)) != matches[0] {
		t.Errorf("local path %s is not learn.Key %s", matches[0], want)
	}

	listed := captureStdout(t, func() {
		if code := learnObservations(context.Background(), f.env, "claude"); code != 0 {
			t.Errorf("learn observations = %d", code)
		}
	})
	if n := strings.Count(listed, `"session":"sess-`); n != 3 {
		t.Errorf("learn observations listed %d waiting sessions, want 3:\n%s", n, listed)
	}
	r, err := buildReport(context.Background(), f.env, "30d")
	if err != nil || len(r.Findings) == 0 {
		t.Errorf("report over waiting observations = %d findings, %v", len(r.Findings), err)
	}

	upload(t, f)
	keys, _ := memRemote.List(context.Background(), "analysis/")
	if len(keys) != 3 {
		t.Fatalf("remote after upload = %v", keys)
	}
	for _, k := range keys {
		local, err := os.ReadFile(filepath.Join(config.PendingDir(f.home), filepath.FromSlash(k)))
		if err != nil {
			t.Fatal(err)
		}
		r, _ := memRemote.Get(context.Background(), k)
		remote, _ := io.ReadAll(r)
		if string(remote) != string(local) {
			t.Errorf("%s: remote bytes differ from local", k)
		}
	}
	if n := waiting(t, f); n != 0 {
		t.Errorf("waiting after upload = %d", n)
	}
}

func TestProposeStopsWhileObservationsWait(t *testing.T) {
	f := remoteFixture(t)
	observeOnce(t, f)
	if n := waiting(t, f); n != 3 {
		t.Fatalf("waiting = %d, want 3", n)
	}
	const want = "3 objects wait; run `recall sync`"
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"propose and learn run", func() error {
			var code int
			errOut := captureStderr(t, func() {
				captureStdout(t, func() { code, _ = learnRun(context.Background(), f.env, f.runner, "", reduceWindow{}, false) })
			})
			if code == 0 {
				return nil
			}
			return errors.New(errOut)
		}},
		{"learn reduce", func() error {
			_, _, err := reduce(context.Background(), f.env, f.runner, "", false, reduceWindow{}, false)
			return err
		}},
		{"learn reduce --dry-run", func() error {
			var err error
			captureStdout(t, func() { _, _, err = reduce(context.Background(), f.env, f.runner, "", true, reduceWindow{}, false) })
			return err
		}},
		{"learn reduce --force", func() error {
			_, _, err := reduce(context.Background(), f.env, f.runner, "", false, reduceWindow{}, true)
			return err
		}},
	} {
		before := len(f.runner.calls)
		err := tc.run()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, want)
		}
		if n := len(f.runner.calls) - before; n != 0 {
			t.Errorf("%s: %d model calls, want 0", tc.name, n)
		}
	}

	upload(t, f)
	var err error
	captureStderr(t, func() {
		captureStdout(t, func() { _, _, err = reduce(context.Background(), f.env, f.runner, "", false, reduceWindow{}, false) })
	})
	if err != nil {
		t.Errorf("propose after the upload: %v", err)
	}
}

func TestFSStoreNeverWaits(t *testing.T) {
	f := newHookFixture(t)
	f.transcript(t, "sess-0", f.clk.T.Add(-24*time.Hour))
	if calls, out, _ := observeOnce(t, f); calls != 1 || strings.Contains(out, "wait") {
		t.Errorf("observe on fs: %d calls, output:\n%s", calls, out)
	}
	if n := waiting(t, f); n != 0 {
		t.Errorf("waiting on fs = %d", n)
	}
	var err error
	captureStderr(t, func() {
		captureStdout(t, func() { _, _, err = reduce(context.Background(), f.env, f.runner, "", false, reduceWindow{}, false) })
	})
	if err != nil {
		t.Errorf("propose on fs: %v", err)
	}
}

func TestObserveSpendsEachCallOnceWithTheRemoteDown(t *testing.T) {
	f := remoteFixture(t)
	remoteDown = true
	calls, _, errOut := observeOnce(t, f)
	if calls != 3 {
		t.Fatalf("first pass made %d calls, want 3", calls)
	}
	if !strings.Contains(errOut, "warning") || !strings.Contains(errOut, "NoSuchBucket") {
		t.Errorf("no warning about the store:\n%s", errOut)
	}
	if calls, _, _ := observeOnce(t, f); calls != 0 {
		t.Errorf("second pass made %d calls, want 0", calls)
	}
	remoteDown = false
	upload(t, f)
	if keys, _ := memRemote.List(context.Background(), "analysis/"); len(keys) != 3 {
		t.Errorf("remote after upload = %v", keys)
	}
	if calls, _, _ := observeOnce(t, f); calls != 0 {
		t.Errorf("third pass made %d calls, want 0", calls)
	}
}

func TestObserveReusesObservationsAlreadyInTheStore(t *testing.T) {
	f := remoteFixture(t)
	// A pass from before observations landed on disk first wrote straight to the store.
	f.env.settings.Store.Kind = "fs"
	f.env.settings.Store.Path = filepath.Join(f.home, "old-store")
	observeOnce(t, f)
	old, err := store.NewFS(f.env.settings.Store.Path)
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := old.List(context.Background(), "analysis/")
	for _, k := range keys {
		r, _ := old.Get(context.Background(), k)
		if err := memRemote.PutIfAbsent(context.Background(), k, r); err != nil {
			t.Fatal(err)
		}
		r.Close()
	}
	f.env.settings.Store.Kind = "memtest"
	if calls, _, _ := observeOnce(t, f); calls != 0 {
		t.Errorf("pass over a store holding the observations made %d calls, want 0", calls)
	}
	if n := waiting(t, f); n != 0 {
		t.Errorf("waiting = %d, want 0", n)
	}
}

// propose stops before rules sync opens the store.
func TestProposeStopsBeforeOpeningTheStore(t *testing.T) {
	f := remoteFixture(t)
	observeOnce(t, f)
	memOpens = 0
	errOut := captureStderr(t, func() {
		captureStdout(t, func() {
			if code, _ := learnRun(context.Background(), f.env, f.runner, "", reduceWindow{}, false); code == 0 {
				t.Error("propose went ahead with objects waiting")
			}
		})
	})
	if !strings.Contains(errOut, "3 objects wait; run `recall sync`") {
		t.Errorf("stop = %q", errOut)
	}
	if memOpens != 0 {
		t.Errorf("propose opened the store %d time(s) before stopping", memOpens)
	}
}

func TestReportAndObservationsWarnWhenTheStoreIsDown(t *testing.T) {
	f := remoteFixture(t)
	observeOnce(t, f)
	remoteDown = true
	errOut := captureStderr(t, func() {
		if _, err := buildReport(context.Background(), f.env, "30d"); err != nil {
			t.Errorf("report: %v", err)
		}
	})
	if !strings.Contains(errOut, "the store didn't answer") || !strings.Contains(errOut, "NoSuchBucket") {
		t.Errorf("report gave no warning:\n%s", errOut)
	}
	errOut = captureStderr(t, func() {
		captureStdout(t, func() {
			if code := learnObservations(context.Background(), f.env, "claude"); code != 0 {
				t.Errorf("learn observations = %d", code)
			}
		})
	})
	if !strings.Contains(errOut, "the store didn't answer") {
		t.Errorf("learn observations gave no warning:\n%s", errOut)
	}
}

// tokenRunner observes a token in what it returns.
type tokenRunner struct{ scriptedRunner }

func (r *tokenRunner) Run(ctx context.Context, c model.Call, input string) (string, model.Usage, error) {
	r.mu.Lock()
	r.calls = append(r.calls, input)
	r.mu.Unlock()
	return "Observation[context]: the deploy key is " + fakeToken + "\n", model.Usage{}, nil
}

// fakeToken has a token's shape and is no credential.
var fakeToken = "ghp_" + strings.Repeat("A1b2", 9)

func TestPendingFilesHoldRedactedText(t *testing.T) {
	f := remoteFixture(t)
	f.env.settings.Archive.Redact = true
	r := &tokenRunner{}
	captureStderr(t, func() {
		captureStdout(t, func() { learnMap(context.Background(), f.env, r, "30d", 0, false, false, 0, nil) })
	})
	if len(r.calls) == 0 {
		t.Fatal("no model call")
	}
	n := 0
	err := filepath.WalkDir(config.PendingDir(f.home), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		n++
		if strings.Contains(string(data), fakeToken) {
			t.Errorf("%s holds the token unredacted", path)
		}
		return nil
	})
	if err != nil || n == 0 {
		t.Errorf("walked %d pending files, %v", n, err)
	}
}

// One record per host and session, the newest, across local and remote.
func TestListObservationsKeepsTheNewestAcrossCopies(t *testing.T) {
	ctx := context.Background()
	remote := store.NewMem()
	home := t.TempDir()
	p, err := store.NewPending(config.PendingDir(home), config.UploadedDir(home), remote)
	if err != nil {
		t.Fatal(err)
	}
	put := func(st store.Store, key, observedAt string) {
		t.Helper()
		data, _ := json.Marshal(learn.SessionObservations{Harness: "claude", Host: "laptop-a", Session: "s1", ObservedAt: observedAt})
		if err := st.PutIfAbsent(ctx, key, strings.NewReader(string(data))); err != nil {
			t.Fatal(err)
		}
	}
	put(remote, "analysis/claude/s1-old.json", "2026-11-01T00:00:00Z")
	put(p, "analysis/claude/s1-new.json", "2026-11-10T00:00:00Z")
	put(remote, "analysis/claude/s1-new.json", "2026-11-10T00:00:00Z")
	got, err := learn.ListObservations(ctx, p, "claude")
	if err != nil || len(got) != 1 || got[0].ObservedAt != "2026-11-10T00:00:00Z" {
		t.Errorf("observations = %+v, %v; want only the newest", got, err)
	}
}
