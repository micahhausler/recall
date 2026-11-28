package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/harness/adapters"
	"github.com/micahhausler/recall/internal/learn"
	"github.com/micahhausler/recall/internal/recur"
	"github.com/micahhausler/recall/internal/review"
	"github.com/micahhausler/recall/internal/store"
)

var statusNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

const statusSelf = "/x/bin/recall"

type statusFixture struct {
	home string
	env  *archiveEnv
	deps statusDeps
	st   *store.Mem
	tab  string
}

// fakeStatusCrontab puts a crontab backed by a temp file first on PATH.
func fakeStatusCrontab(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nF=\"$(dirname \"$0\")/tab\"\nif [ \"$1\" = \"-l\" ]; then [ -f \"$F\" ] && exec cat \"$F\"; echo \"no crontab\" >&2; exit 1; fi\ncat > \"$F\"\n"
	if err := os.WriteFile(filepath.Join(dir, "crontab"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return filepath.Join(dir, "tab")
}

func newStatusFixture(t *testing.T, toml string) *statusFixture {
	t.Helper()
	// Status reads RECALL_CONFIG into the jobs it compares.
	t.Setenv(config.EnvVar, "")
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".config", "recall", "config.toml")
	if toml != "" {
		if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(base, []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, err := config.Load(base)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := c.Settings(home, "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	hs, err := adapters.Enabled(settings)
	if err != nil {
		t.Fatal(err)
	}
	f := &statusFixture{home: home, st: store.NewMem(), tab: fakeStatusCrontab(t)}
	f.env = &archiveEnv{home: home, settings: settings, harnesses: hs, clk: &clock.Fake{T: statusNow}}
	f.deps = statusDeps{
		base:      base,
		scheduler: "cron",
		self:      statusSelf,
		loc:       time.UTC,
		openStore: func(context.Context, *archiveEnv) (store.Store, error) { return f.st, nil },
	}
	return f
}

func (f *statusFixture) run(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	writeStatus(context.Background(), &b, f.env, f.deps)
	return b.String()
}

// installJobs syncs the declared jobs into the fake crontab.
func (f *statusFixture) installJobs(t *testing.T) {
	t.Helper()
	setup, err := recur.LoadSetup(f.env.settings, statusSelf, "", f.home)
	if err != nil {
		t.Fatal(err)
	}
	sched, err := recur.New("cron", setup)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sched.Sync(f.env.settings.Recur.Jobs, false, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func writeAt(t *testing.T, path, data string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func wantLines(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w+"\n") {
			t.Errorf("status lacks line %q:\n%s", w, out)
		}
	}
}

func TestStatusFreshMachine(t *testing.T) {
	f := newStatusFixture(t, "")
	out := f.run(t)
	wantLines(t, out,
		"config:    ~/.config/recall/config.toml (missing)",
		"host:      laptop-a",
		"store:     fs, ~/recall-store",
		"agents:    claude",
		"           off: recall config set harness.claude.enabled false",
		"proposing: none yet; recall init --claim makes this machine the proposing one",
		"sync:      never run; recall sync runs each step that's due",
		"timer:     none",
		"archive:   off",
		"observe:   off",
		"propose:   off",
		"proposals: none pending",
	)
	if strings.Contains(out, f.home) {
		t.Errorf("status prints the home directory:\n%s", out)
	}
}

func TestStatusHookMachine(t *testing.T) {
	f := newStatusFixture(t, "[hooks]\nmap = true\narchive = true\nlearn_every = 7\n")
	dir := hookStateDir(f.env)
	writeAt(t, filepath.Join(dir, "hook-archive.stamp"), "", time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC))
	writeAt(t, filepath.Join(dir, "hook-learn-tried.stamp"), "", time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC))
	writeAt(t, filepath.Join(dir, "hook.log"),
		"--- 2026-11-18T09:00:00Z recall hook work\nmap: 1 session\n--- 2026-11-19T10:30:00Z recall hook work\n", statusNow)
	if err := claimReducer(context.Background(), f.st, "laptop-a"); err != nil {
		t.Fatal(err)
	}
	out := f.run(t)
	wantLines(t, out,
		"proposing: this machine",
		"timer:     none",
		"archive:   session-end hook, last tried 2026-11-18 09:00",
		"           off: recall config set hooks.archive false",
		"observe:   session-end hook, last tried 2026-11-19 10:30",
		"           off: recall config set hooks.map false",
		"propose:   session-end hook, last tried 2026-11-17 08:00",
		"           off: recall config set hooks.learn_every 0",
	)
}

func TestStatusRecurMachine(t *testing.T) {
	toml := `[recur.jobs.weekly]
schedule = "0 12 * * 1"
command = "recall archive && recall observe"

[recur.jobs.propose]
schedule = "0 13 * * 1"
command = "recall learn run"

[hooks]
map = true
`
	f := newStatusFixture(t, toml)
	f.installJobs(t)
	writeAt(t, filepath.Join(recur.StateDir(f.home), "weekly.log"),
		"=== 2026-11-09T12:00:00Z start weekly: x\n=== 2026-11-09T12:01:00Z exit 0\n"+
			"=== 2026-11-16T12:00:00Z start weekly: x\nboom\n=== 2026-11-16T12:02:00Z exit 3\n", statusNow)
	out := f.run(t)
	wantLines(t, out,
		"timer:     cron, 2 jobs installed",
		"archive:   timer job weekly (0 12 * * 1), last run 2026-11-16 12:00, exit 3",
		"           off: recall config set recur.jobs.weekly.enabled false && recall recur sync",
		"           turning off job weekly also turns off observe",
		"observe:   timer job weekly (0 12 * * 1), last run 2026-11-16 12:00, exit 3",
		"           off: recall config set recur.jobs.weekly.enabled false && recall config set hooks.map false && recall recur sync",
		"           turning off job weekly also turns off archive",
		"propose:   timer job propose (0 13 * * 1), never run",
		"           off: recall config set recur.jobs.propose.enabled false && recall recur sync",
	)

	// A declared job the crontab lacks is drift.
	if err := os.WriteFile(f.tab, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wantLines(t, f.run(t), "timer:     cron, out of date (2 changes); recall recur sync installs it")
	if got, _ := os.ReadFile(f.tab); len(got) != 0 {
		t.Errorf("status wrote the crontab while it was out of date:\n%s", got)
	}
}

func TestStatusHookNoAgentRunsTheLoop(t *testing.T) {
	f := newStatusFixture(t, "[harness.kiro]\n\n[harness.claude]\nenabled = false\n\n[hooks]\nmap = true\n")
	wantLines(t, f.run(t), "observe:   not running; hooks.map is on, but no enabled agent's session-end hook runs the loop")
}

func TestStatusS3Store(t *testing.T) {
	f := newStatusFixture(t, "[store]\nkind = \"s3\"\n\n[store.s3]\nbucket = \"example-bucket\"\nregion = \"us-west-2\"\n")
	wantLines(t, f.run(t), "store:     s3, s3://example-bucket (us-west-2)")
}

// An s3 store with no bucket fails to load, and the error names the fix.
func TestStatusCommandS3WithoutBucket(t *testing.T) {
	tempHome(t, "[store]\nkind = \"s3\"\n")
	fakeStatusCrontab(t)
	code, _, errText := capture(t, "status")
	if code == 0 || !strings.Contains(errText, "recall config set store.s3.bucket <name>") {
		t.Errorf("recall status = %d, %q; want the unset-bucket fix", code, errText)
	}
	if home, _ := os.UserHomeDir(); strings.Contains(errText, home) {
		t.Errorf("recall status printed the home directory: %q", errText)
	}
}

// failingStore fails every call and counts them.
type failingStore struct {
	store.Store
	calls int
}

func (s *failingStore) fail() error {
	s.calls++
	return errors.New("connection refused after 3 attempts")
}
func (s *failingStore) Get(context.Context, string) (io.ReadCloser, error) { return nil, s.fail() }
func (s *failingStore) List(context.Context, string) ([]string, error)     { return nil, s.fail() }
func (s *failingStore) Head(context.Context, string) (store.Info, error) {
	return store.Info{}, s.fail()
}

func TestStatusStoreFailsOnFirstRead(t *testing.T) {
	f := newStatusFixture(t, "")
	fs := &failingStore{}
	f.deps.openStore = func(context.Context, *archiveEnv) (store.Store, error) { return fs, nil }
	out := f.run(t)
	wantLines(t, out,
		"store:     fs, ~/recall-store; unreachable: connection refused after 3 attempts",
		"proposing: unknown; the store is unreachable",
		"proposals: unknown; the store is unreachable",
	)
	if n := strings.Count(out, "connection refused"); n != 1 {
		t.Errorf("the store error printed %d times, want once:\n%s", n, out)
	}
	if fs.calls != 1 {
		t.Errorf("status made %d store calls after the first failed, want 1", fs.calls)
	}

	// A local copy is counted, and labeled as this machine's.
	local := &learn.Ledger{Reviews: []learn.Review{{Title: "a", Where: "w1", Verdict: review.Pending}}}
	if err := local.Save(config.LedgerPath(f.home)); err != nil {
		t.Fatal(err)
	}
	wantLines(t, f.run(t), "proposals: 1 pending in this machine's copy; the store is unreachable")
}

func TestScrubStatus(t *testing.T) {
	cases := map[string]string{
		"arn:aws:sts::123456789012:assumed-role/run/you@example.com is not authorized": "<arn> is not authorized",
		"account 123456789012 denied":                "account <account> denied",
		"/home/you/x and /home/you2/x and /home/you": "~/x and /home/you2/x and ~",
		"open /home/you: denied; /home/you.bak/y":    "open ~: denied; /home/you.bak/y",
	}
	for in, want := range cases {
		if got := scrubStatus(in, "/home/you"); got != want {
			t.Errorf("scrubStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusNamesTheProposingMachine(t *testing.T) {
	f := newStatusFixture(t, "")
	if err := claimReducer(context.Background(), f.st, "laptop-b"); err != nil {
		t.Fatal(err)
	}
	wantLines(t, f.run(t), "proposing: laptop-b, not this machine")
}

func TestStatusCountsPendingProposals(t *testing.T) {
	f := newStatusFixture(t, "")
	l := &learn.Ledger{Reviews: []learn.Review{
		{Title: "a", Where: "w1", Verdict: review.Pending},
		{Title: "b", Where: "w2", Verdict: review.Pending},
		{Title: "c", Where: "w3", Verdict: "merged"},
	}}
	if _, err := learn.PutStoreLedger(context.Background(), f.st, l, statusNow); err != nil {
		t.Fatal(err)
	}
	wantLines(t, f.run(t), "proposals: 2 pending; recall proposals lists them")

	// With no store ledger, the local one counts.
	f.st = store.NewMem()
	local := &learn.Ledger{Reviews: []learn.Review{{Title: "a", Where: "w1", Verdict: review.Pending}}}
	if err := local.Save(config.LedgerPath(f.home)); err != nil {
		t.Fatal(err)
	}
	wantLines(t, f.run(t), "proposals: 1 pending; recall proposals lists them")

	// On the proposing machine a newer local copy wins over the store's.
	if err := claimReducer(context.Background(), f.st, "laptop-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := learn.PutStoreLedger(context.Background(), f.st, &learn.Ledger{}, statusNow); err != nil {
		t.Fatal(err)
	}
	wantLines(t, f.run(t), "proposals: 1 pending; recall proposals lists them")
}

func TestStatusUnreachableStore(t *testing.T) {
	f := newStatusFixture(t, "[hooks]\nmap = true\n")
	f.deps.openStore = func(context.Context, *archiveEnv) (store.Store, error) {
		return nil, errors.New("arn:aws:iam::123456789012:role/run is not authorized under " + f.home)
	}
	out := f.run(t)
	wantLines(t, out,
		"store:     fs, ~/recall-store; unreachable: <arn> is not authorized under ~",
		"proposing: unknown; the store is unreachable",
		"observe:   session-end hook, never run",
		"proposals: unknown; the store is unreachable",
	)
	if strings.Contains(out, "123456789012") {
		t.Errorf("status prints an account ID:\n%s", out)
	}
}

// A fs store that doesn't exist yet stays uncreated.
func TestStatusLeavesMissingFSStore(t *testing.T) {
	f := newStatusFixture(t, "")
	f.deps.openStore = nil
	wantLines(t, f.run(t), "store:     fs, ~/recall-store (not created yet)")
	if exists(f.env.settings.Store.Path) {
		t.Error("status created the fs store")
	}
}

type fileState struct {
	data string
	mod  time.Time
	mode fs.FileMode
}

func snapshot(t *testing.T, root string) map[string]fileState {
	t.Helper()
	m := map[string]fileState{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		s := fileState{mod: info.ModTime(), mode: info.Mode()}
		if !d.IsDir() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			s.data = string(data)
		}
		m[p] = s
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func storeSnapshot(t *testing.T, st store.Store) map[string]string {
	t.Helper()
	ctx := context.Background()
	keys, err := st.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, k := range keys {
		r, err := st.Get(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(r)
		r.Close()
		m[k] = string(data)
	}
	return m
}

func TestStatusChangesNothing(t *testing.T) {
	toml := "[recur.jobs.weekly]\nschedule = \"0 12 * * 1\"\ncommand = \"recall archive\"\n\n[hooks]\nmap = true\nlearn_every = 7\n"
	f := newStatusFixture(t, toml)
	f.installJobs(t)
	writeAt(t, filepath.Join(hookStateDir(f.env), "hook.log"), "--- 2026-11-18T09:00:00Z recall hook work\n", statusNow)
	if err := claimReducer(context.Background(), f.st, "laptop-b"); err != nil {
		t.Fatal(err)
	}
	tab, _ := os.ReadFile(f.tab)
	home, st := snapshot(t, f.home), storeSnapshot(t, f.st)
	f.run(t)
	if got := snapshot(t, f.home); !reflect.DeepEqual(got, home) {
		t.Errorf("status changed the home directory:\nbefore %v\nafter  %v", home, got)
	}
	if got := storeSnapshot(t, f.st); !reflect.DeepEqual(got, st) {
		t.Errorf("status changed the store:\nbefore %v\nafter  %v", st, got)
	}
	if got, _ := os.ReadFile(f.tab); string(got) != string(tab) {
		t.Errorf("status changed the crontab:\nbefore %q\nafter  %q", tab, got)
	}
}

func TestStatusCommandWithNoConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(config.EnvVar, filepath.Join(home, "nowhere", "config.toml"))
	fakeStatusCrontab(t)
	code, out, errText := capture(t, "status")
	if code != 0 {
		t.Fatalf("recall status = %d, stderr %q", code, errText)
	}
	for _, w := range []string{"host:", "store:", "timer:", "proposals:"} {
		if !strings.Contains(out, w) {
			t.Errorf("recall status lacks %q:\n%s", w, out)
		}
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("recall status wrote into an empty home: %v", entries)
	}
}

func TestStatusCountsWaitingObservations(t *testing.T) {
	f := newStatusFixture(t, "[store]\nkind = \"s3\"\n\n[store.s3]\nbucket = \"example-bucket\"\nregion = \"us-west-2\"\n")
	if strings.Contains(f.run(t), "waiting:") {
		t.Error("status shows a waiting line with nothing waiting")
	}
	writeAt(t, filepath.Join(config.PendingDir(f.home), "analysis", "claude", "s1-abc.json"), "{}", statusNow)
	wantLines(t, f.run(t), "waiting:   1 objects wait; run `recall sync`")
}

// Status says who runs each import: unknown with the store down, nobody
// on an unclaimed store.
func TestStatusImports(t *testing.T) {
	const chat = "[harness.chat]\nformat = \"events\"\n\n[harness.chat.import]\ncommand = [\"chat-history\"]\n"
	f := newStatusFixture(t, chat)
	wantLines(t, f.run(t),
		"imports:   run by recall sync on the proposing machine",
		"           chat: skipped here; no machine proposes for this store yet, and recall init --claim makes this one the proposing machine",
		"sync:      never run; recall sync runs each step that's due",
	)
	f.deps.openStore = func(context.Context, *archiveEnv) (store.Store, error) { return nil, errors.New("no route to host") }
	wantLines(t, f.run(t), "           chat: unknown; the store is unreachable")
}
