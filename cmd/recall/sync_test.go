package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	importsv1 "github.com/micahhausler/recall/api/imports/v1"
	"github.com/micahhausler/recall/internal/archive"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/hook"
	"github.com/micahhausler/recall/internal/imports"
	"github.com/micahhausler/recall/internal/model"
	"github.com/micahhausler/recall/internal/store"
)

// rejectUploads makes the "rejecttest" store refuse analysis/ writes.
var rejectUploads bool

type rejectStore struct{ store.Store }

func (r rejectStore) PutIfAbsent(ctx context.Context, key string, rd io.Reader) error {
	if rejectUploads && strings.HasPrefix(key, store.AnalysisPrefix) {
		return errors.New("operation error S3: PutObject, api error AccessDenied")
	}
	return r.Store.PutIfAbsent(ctx, key, rd)
}

func init() {
	store.Register("rejecttest", func(context.Context, config.StoreSettings) (store.Store, error) {
		return rejectStore{memRemote}, nil
	})
}

// failingRunner fails the reduce, or every call; safe for observe's
// parallel workers.
type failingRunner struct {
	*scriptedRunner
	reduce, all bool
	mu          sync.Mutex
}

func (r *failingRunner) Run(ctx context.Context, c model.Call, input string) (string, model.Usage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.all || (r.reduce && strings.Contains(input, "# CANDIDATES")) {
		r.calls = append(r.calls, input)
		return "", model.Usage{}, errors.New("model unavailable")
	}
	return r.scriptedRunner.Run(ctx, c, input)
}

const syncChat = "[harness.chat]\nformat = \"events\"\n\n[harness.chat.import]\ncommand = [\"chat-history\"]\n"

// syncFixture is one machine on the shared memRemote store.
type syncFixture struct {
	*hookFixture
	src *fakeSources
}

// newSyncStore resets the store every syncFixture shares.
func newSyncStore(t *testing.T) {
	t.Helper()
	memRemote, remoteDown, rejectUploads = store.NewMem(), false, false
	t.Cleanup(func() { remoteDown, rejectUploads = false, false })
}

func newSyncFixture(t *testing.T, kind, host, toml string) *syncFixture {
	t.Helper()
	t.Setenv(config.EnvVar, "")
	f := newHookFixtureWith(t, "[learn]\nreview = \"branch\"\n\n"+toml).noOwnerEvidence()
	f.env.settings.Host = host
	if kind != "" {
		f.env.settings.Store.Kind = kind
	}
	src := &fakeSources{fakes: map[string]*imports.Fake{}}
	for name, h := range f.env.settings.Harnesses {
		if h.Import != nil {
			src.fakes[name] = &imports.Fake{Status: importsv1.Status{}}
		}
	}
	return &syncFixture{hookFixture: f, src: src}
}

func claim(t *testing.T, host string) {
	t.Helper()
	if err := claimReducer(context.Background(), memRemote, host); err != nil {
		t.Fatal(err)
	}
}

func (f *syncFixture) sync(t *testing.T, force bool) (code int, stdout, stderr string) {
	t.Helper()
	stderr = captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			code = runSync(context.Background(), f.env, syncOpts{force: force}, f.src.open)
		})
	})
	return code, stdout, stderr
}

// reduces counts the reduce inputs the runner saw.
func reduces(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, "# CANDIDATES") {
			n++
		}
	}
	return n
}

func observed(calls []string, text string) bool {
	for _, c := range calls {
		if !strings.Contains(c, "# CANDIDATES") && strings.Contains(c, text) {
			return true
		}
	}
	return false
}

func stampAt(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// monday is a Monday morning in the zone the archive runs in.
var monday = time.Date(2026, 11, 23, 9, 0, 0, 0, time.Local)

func hasKey(t *testing.T, key string) bool {
	t.Helper()
	_, err := memRemote.Head(context.Background(), key)
	return err == nil
}

func lastWeek(now time.Time) archive.Week { return archive.RecentWeeks(1, now)[0] }

func order(t *testing.T, out string, marks ...string) {
	t.Helper()
	at := -1
	for _, m := range marks {
		i := strings.Index(out, m)
		if i < 0 || i < at {
			t.Fatalf("%q is missing or out of order in:\n%s", m, out)
		}
		at = i
	}
}

func TestSyncProposingMachineRunsEveryStepAndAnotherOnlyArchivesAndObserves(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")

	a := newSyncFixture(t, "memtest", "laptop-a", syncChat)
	a.clk.T = monday
	a.transcript(t, "sess-a", monday.AddDate(0, 0, -2))
	code, out, errOut := a.sync(t, false)
	if code != 0 {
		t.Fatalf("sync = %d\n%s\n%s", code, out, errOut)
	}
	order(t, out, "import chat:", "archive:", "observe:", "upload:", "apply:")
	if len(a.src.fakes["chat"].Specs) != 1 {
		t.Errorf("imports run = %d, want 1", len(a.src.fakes["chat"].Specs))
	}
	if !hasKey(t, archive.Key("claude", "laptop-a", lastWeek(monday))) {
		t.Error("the week wasn't archived")
	}
	if !observed(a.runner.calls, "always run go vet") || reduces(a.runner.calls) != 1 {
		t.Errorf("observe or propose didn't run: %d calls", len(a.runner.calls))
	}
	if a.pending(t) != 1 || !exists(a.stamp("learn")) {
		t.Error("the proposal wasn't sent or the week not stamped")
	}
	if n := waiting(t, a.hookFixture); n != 0 {
		t.Errorf("%d objects still wait after sync", n)
	}

	b := newSyncFixture(t, "memtest", "laptop-b", syncChat)
	b.clk.T = monday
	b.transcript(t, "sess-b", monday.AddDate(0, 0, -2))
	code, out, errOut = b.sync(t, false)
	if code != 0 {
		t.Fatalf("sync on laptop-b = %d\n%s\n%s", code, out, errOut)
	}
	if n := len(b.src.fakes["chat"].Specs); n != 0 {
		t.Errorf("laptop-b ran the import %d times", n)
	}
	for _, want := range []string{
		"import chat: skipped; laptop-a proposes for this store and runs its imports\n",
		"propose: skipped; laptop-a is the proposing machine for this store\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("laptop-b's sync lacks %q:\n%s", want, out)
		}
	}
	if !hasKey(t, archive.Key("claude", "laptop-b", lastWeek(monday))) {
		t.Error("laptop-b's week wasn't archived")
	}
	if !observed(b.runner.calls, "always run go vet") || reduces(b.runner.calls) != 0 {
		t.Errorf("laptop-b observed %v, reduced %d", observed(b.runner.calls, "go vet"), reduces(b.runner.calls))
	}
	if exists(b.stamp("learn-tried")) {
		t.Error("laptop-b stamped a propose it never ran")
	}

	for _, tc := range []struct {
		f    *syncFixture
		want string
	}{
		{b, "chat: run by laptop-a, skipped here"},
		{a, "chat: last run " + monday.UTC().Format("2006-01-02 15:04") + ", ok"},
	} {
		var sb strings.Builder
		fakeStatusCrontab(t)
		writeStatus(context.Background(), &sb, tc.f.env, statusDeps{
			base: filepath.Join(tc.f.home, ".config", "recall", "config.toml"), scheduler: "cron", self: statusSelf, loc: time.UTC,
			openStore: func(context.Context, *archiveEnv) (store.Store, error) { return memRemote, nil },
		})
		if !strings.Contains(sb.String(), tc.want+"\n") {
			t.Errorf("status on %s lacks %q:\n%s", tc.f.env.settings.Host, tc.want, sb.String())
		}
		if !strings.Contains(sb.String(), "sync:      last run "+monday.UTC().Format("2006-01-02 15:04")) {
			t.Errorf("status on %s lacks the sync line:\n%s", tc.f.env.settings.Host, sb.String())
		}
	}
}

// imported is one session from the Friday before monday.
const imported = `{"session":"s-0001","cwd":"/home/you/src/example","timestamp":"2026-11-20T12:00:00Z","kind":"user_prompt","role":"user","text":"run the imported tests"}
{"session":"s-0001","cwd":"/home/you/src/example","timestamp":"2026-11-20T12:00:04Z","kind":"assistant_text","role":"assistant","text":"done"}
`

// writtenAt dates the stored import session as written at when by the
// fake clock; the store dates it by the real write.
func (f *syncFixture) writtenAt(t *testing.T, when time.Time) {
	t.Helper()
	path := filepath.Join(f.env.settings.Harnesses["chat"].Source, "s-0001.jsonl")
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// An import runs every sync from where the last left off; what it stored
// is observed once quiet, and archived with the week it was written in by
// the first sync after that week ends.
func TestSyncImportsBeforeArchiveAndObserve(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	f := newSyncFixture(t, "memtest", "laptop-a", syncChat)
	f.src.fakes["chat"].Files = map[string]string{"one.jsonl": imported}
	f.src.fakes["chat"].Status = importsv1.Status{Sessions: 1}
	f.clk.T = monday

	if code, out, errOut := f.sync(t, false); code != 0 {
		t.Fatalf("sync = %d\n%s\n%s", code, out, errOut)
	}
	f.writtenAt(t, monday)

	// Past the quiet period, the next sync runs the import again and
	// observes what the first stored.
	f.clk.T = monday.Add(7 * time.Hour)
	if code, out, errOut := f.sync(t, false); code != 0 {
		t.Fatalf("second sync = %d\n%s\n%s", code, out, errOut)
	}
	specs := f.src.fakes["chat"].Specs
	if len(specs) != 2 {
		t.Fatalf("imports run = %d, want 2", len(specs))
	}
	if want := monday.Add(-time.Hour).UTC().Format(time.RFC3339); specs[1].Since != want {
		t.Errorf("second since = %q, want %q", specs[1].Since, want)
	}
	if !observed(f.runner.calls, "run the imported tests") {
		t.Error("the imported session wasn't observed once quiet")
	}

	// Written on Monday, it is in that week's tarball, which the next
	// Monday's sync uploads.
	written := lastWeek(monday.AddDate(0, 0, 7))
	if hasKey(t, archive.Key("chat", "laptop-a", written)) {
		t.Fatal("the week it was written in was archived before it ended")
	}
	f.clk.T = monday.AddDate(0, 0, 7)
	if code, out, errOut := f.sync(t, false); code != 0 {
		t.Fatalf("next week's sync = %d\n%s\n%s", code, out, errOut)
	}
	if key := archive.Key("chat", "laptop-a", written); !hasKey(t, key) {
		t.Errorf("the imported session isn't in the tarball of the week it was written in, %s", key)
	}
}

func TestSyncSkipsImportsOnAnUnclaimedStore(t *testing.T) {
	newSyncStore(t)
	f := newSyncFixture(t, "memtest", "laptop-a", syncChat)
	code, out, errOut := f.sync(t, false)
	if code != 0 {
		t.Fatalf("sync = %d\n%s\n%s", code, out, errOut)
	}
	if n := len(f.src.fakes["chat"].Specs); n != 0 {
		t.Errorf("imports run = %d on an unclaimed store", n)
	}
	for _, want := range []string{"import chat: skipped; no machine proposes for this store yet; `recall init --claim` makes this one the proposing machine\n",
		"propose: skipped; no machine proposes for this store yet; `recall init --claim` makes this one the proposing machine\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync lacks %q:\n%s", want, out)
		}
	}
}

// `recall import NAME` by hand runs anywhere, and the next sync archives
// what it stored.
func TestHandImportOnAnotherMachineIsArchivedByItsSync(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	f := newSyncFixture(t, "memtest", "laptop-b", syncChat)
	f.src.fakes["chat"].Files = map[string]string{"one.jsonl": imported}
	f.src.fakes["chat"].Status = importsv1.Status{Sessions: 1}
	f.clk.T = monday.AddDate(0, 0, -3)
	captureStdout(t, func() {
		if code := runImports(context.Background(), f.env, []string{"chat"}, importFlags{}, f.src.open); code != 0 {
			t.Errorf("import = %d", code)
		}
	})
	if n := len(f.src.fakes["chat"].Specs); n != 1 {
		t.Fatalf("hand import ran %d times", n)
	}
	f.writtenAt(t, monday.AddDate(0, 0, -3))
	f.clk.T = monday
	if code, out, errOut := f.sync(t, false); code != 0 {
		t.Fatalf("sync = %d\n%s\n%s", code, out, errOut)
	}
	if n := len(f.src.fakes["chat"].Specs); n != 1 {
		t.Errorf("the sync on laptop-b ran the import too")
	}
	if !hasKey(t, archive.Key("chat", "laptop-b", lastWeek(f.clk.T))) {
		t.Error("the hand import wasn't archived by the next sync")
	}
}

func TestSyncStepsFailApart(t *testing.T) {
	t.Run("one import fails", func(t *testing.T) {
		newSyncStore(t)
		claim(t, "laptop-a")
		f := newSyncFixture(t, "memtest", "laptop-a", syncChat+"\n[harness.notes]\nformat = \"events\"\n\n[harness.notes.import]\ncommand = [\"notes\"]\n")
		f.src.fakes["chat"].Err = errors.New("exit status 1")
		f.clk.T = monday
		f.transcript(t, "sess", monday.AddDate(0, 0, -2))
		code, out, errOut := f.sync(t, false)
		if code == 0 {
			t.Errorf("sync with a failed import = 0\n%s", out)
		}
		if len(f.src.fakes["notes"].Specs) != 1 {
			t.Error("the other import didn't run")
		}
		if !strings.Contains(errOut, "recall: import chat:") {
			t.Errorf("the failed import wasn't reported:\n%s", errOut)
		}
		if !hasKey(t, archive.Key("claude", "laptop-a", lastWeek(monday))) || !observed(f.runner.calls, "go vet") {
			t.Error("archive or observe didn't run after the failed import")
		}
	})
	t.Run("every model call fails", func(t *testing.T) {
		newSyncStore(t)
		claim(t, "laptop-a")
		f := newSyncFixture(t, "memtest", "laptop-a", "")
		f.env.runner = &failingRunner{scriptedRunner: f.runner, all: true}
		f.clk.T = monday
		f.transcript(t, "sess", monday.AddDate(0, 0, -2))
		code, out, _ := f.sync(t, false)
		if code == 0 {
			t.Errorf("sync with a failing model = 0\n%s", out)
		}
		if !hasKey(t, archive.Key("claude", "laptop-a", lastWeek(monday))) {
			t.Error("the archive didn't upload with the model down")
		}
	})
}

func TestSyncDueRules(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	f := newSyncFixture(t, "memtest", "laptop-a", syncChat)
	f.clk.T = monday
	f.transcript(t, "sess", monday.AddDate(0, 0, -2))
	if code, out, errOut := f.sync(t, false); code != 0 {
		t.Fatalf("sync = %d\n%s\n%s", code, out, errOut)
	}
	calls := len(f.runner.calls)
	tried := stampAt(t, f.stamp("learn-tried"))

	f.clk.T = monday.Add(time.Hour)
	code, out, errOut := f.sync(t, false)
	if code != 0 {
		t.Fatalf("second sync = %d\n%s\n%s", code, out, errOut)
	}
	if len(f.runner.calls) != calls {
		t.Errorf("a second sync an hour later made %d model call(s)", len(f.runner.calls)-calls)
	}
	if len(f.src.fakes["chat"].Specs) != 2 {
		t.Error("the second sync didn't run the import")
	}
	for _, want := range []string{"archive: nothing due", "observe: 0 observed", "propose: not due"} {
		if !strings.Contains(out, want) {
			t.Errorf("second sync lacks %q:\n%s", want, out)
		}
	}

	f.clk.T = monday.AddDate(0, 0, 6)
	f.sync(t, false)
	if got := stampAt(t, f.stamp("learn-tried")); !got.Equal(tried) {
		t.Error("a sync 6 days later proposed")
	}
	f.clk.T = monday.AddDate(0, 0, 7)
	f.sync(t, false)
	if got := stampAt(t, f.stamp("learn-tried")); !got.Equal(f.clk.T) {
		t.Error("a sync 7 days later didn't propose")
	}
}

func TestSyncRetriesAFailedProposeADayLater(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	f := newSyncFixture(t, "memtest", "laptop-a", "")
	fr := &failingRunner{scriptedRunner: f.runner, reduce: true}
	f.env.runner = fr
	f.clk.T = monday
	f.transcript(t, "sess", monday.AddDate(0, 0, -2))
	if code, _, _ := f.sync(t, false); code == 0 {
		t.Error("a failed propose exited 0")
	}
	if reduces(f.runner.calls) != 1 || exists(f.stamp("learn")) {
		t.Fatal("the failing propose didn't run once, or was stamped done")
	}
	f.clk.T = monday.Add(time.Hour)
	f.sync(t, false)
	if reduces(f.runner.calls) != 1 {
		t.Error("a sync an hour later retried the failed propose")
	}
	fr.reduce = false
	f.clk.T = monday.Add(25 * time.Hour)
	if code, out, errOut := f.sync(t, false); code != 0 {
		t.Fatalf("sync a day later = %d\n%s\n%s", code, out, errOut)
	}
	if reduces(f.runner.calls) != 2 || f.pending(t) != 1 {
		t.Error("a sync a day later didn't retry the propose")
	}
}

func TestSyncCarriesTheHookWeekOver(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	f := newSyncFixture(t, "memtest", "laptop-a", "")
	f.clk.T = monday
	if err := hook.Touch(f.stamp("learn"), monday.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.transcript(t, "sess", monday.AddDate(0, 0, -2))
	f.sync(t, false)
	if reduces(f.runner.calls) != 0 || exists(f.stamp("learn-tried")) {
		t.Error("a machine whose hook proposed 2 days ago proposed again")
	}
}

func TestSyncForce(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	a := newSyncFixture(t, "memtest", "laptop-a", syncChat)
	a.clk.T = monday
	a.transcript(t, "sess", monday.AddDate(0, 0, -2))
	a.sync(t, false)
	a.clk.T = monday.Add(time.Hour)
	a.transcript(t, "sess-new", monday.Add(30*time.Minute))
	if code, out, errOut := a.sync(t, true); code != 0 {
		t.Fatalf("sync --force = %d\n%s\n%s", code, out, errOut)
	}
	if !stampAt(t, a.stamp("learn-tried")).Equal(a.clk.T) {
		t.Error("--force didn't propose inside the week")
	}
	if n := len(a.runner.calls); n < 3 {
		t.Errorf("--force made %d model calls; want the active session observed and a propose", n)
	}

	b := newSyncFixture(t, "memtest", "laptop-b", syncChat)
	b.clk.T = monday
	b.sync(t, true)
	if reduces(b.runner.calls) != 0 || len(b.src.fakes["chat"].Specs) != 0 {
		t.Error("--force proposed or imported off the proposing machine")
	}
}

// F4: a Monday sync uploads the week before and observes its sessions.
func TestSyncMondayCoversTheWeekItArchives(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	f := newSyncFixture(t, "memtest", "laptop-a", "")
	f.clk.T = monday
	sat := time.Date(2026, 11, 21, 15, 0, 0, 0, time.Local)
	f.transcript(t, "sess-f4", sat)
	if code, out, errOut := f.sync(t, false); code != 0 {
		t.Fatalf("sync = %d\n%s\n%s", code, out, errOut)
	}
	week := lastWeek(monday)
	if week.String() != "2026-W47" {
		t.Fatalf("the week before Monday 2026-11-23 is %s", week)
	}
	rc, err := memRemote.Get(context.Background(), archive.Key("claude", "laptop-a", week))
	if err != nil {
		t.Fatalf("this sync uploaded no %s tarball: %v", week, err)
	}
	defer rc.Close()
	var claude harness.Harness
	for _, h := range f.env.harnesses {
		if h.Name() == "claude" {
			claude = h
		}
	}
	evs, err := harness.ReadTarZstFrom(claude, rc, "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		found = found || e.Session == "sess-f4"
	}
	if !found {
		t.Error("the tarball lacks the Saturday session")
	}
	if !observed(f.runner.calls, "always run go vet") {
		t.Error("the same sync didn't observe the Saturday session")
	}
}

func TestSyncWaitsOutTheQuietPeriod(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	f := newSyncFixture(t, "memtest", "laptop-a", "")
	f.clk.T = monday
	f.transcript(t, "sess", monday.Add(-time.Hour))
	f.sync(t, false)
	if observed(f.runner.calls, "go vet") {
		t.Error("a session active an hour ago was observed")
	}
	f.clk.T = monday.Add(6 * time.Hour)
	f.sync(t, false)
	if !observed(f.runner.calls, "go vet") {
		t.Error("a later sync didn't observe the session once it was quiet")
	}
}

func TestSyncHeldLockRunsNothing(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	f := newSyncFixture(t, "memtest", "laptop-a", syncChat)
	f.clk.T = monday
	f.transcript(t, "sess", monday.AddDate(0, 0, -2))
	release, ok := hook.Lock(filepath.Join(hookStateDir(f.env), "hook.lock"), workerStale, monday)
	if !ok {
		t.Fatal("lock")
	}
	defer release()
	code, _, errOut := f.sync(t, false)
	if code == 0 || !strings.Contains(errOut, "nothing ran") {
		t.Errorf("sync under a held lock = %d: %s", code, errOut)
	}
	if len(f.runner.calls) != 0 || len(f.src.fakes["chat"].Specs) != 0 || hasKey(t, archive.Key("claude", "laptop-a", lastWeek(monday))) {
		t.Error("sync ran a step under a held lock")
	}
}

func TestHookWorkerSkipsWhatASyncJobRuns(t *testing.T) {
	t.Setenv(config.EnvVar, "")
	f := newHookFixtureWith(t, "[learn]\nreview = \"branch\"\n\n[hooks]\nmap = true\narchive = true\nlearn_every = 7\n\n"+
		"[recur.jobs.recall-sync]\nschedule = \"0 12 * * 1\"\ncommand = \"recall sync\"\n").noOwnerEvidence()
	f.transcript(t, "sess", f.clk.T.AddDate(0, 0, -2))
	hookWork(f.env)
	if len(f.runner.calls) != 0 || exists(f.stamp("archive")) || exists(f.stamp("learn-tried")) {
		t.Error("the hook worker ran a step the recall sync job runs")
	}
}

func TestSyncUploadsWhatAHandObserveLeft(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	f := newSyncFixture(t, "memtest", "laptop-a", "")
	f.clk.T = monday
	f.transcript(t, "sess", monday.AddDate(0, 0, -2))
	observeOnce(t, f.hookFixture)
	if waiting(t, f.hookFixture) == 0 {
		t.Fatal("nothing waits after a hand observe")
	}
	var code int
	errOut := captureStderr(t, func() {
		captureStdout(t, func() {
			code, _ = learnRun(context.Background(), f.env, f.runner, "", reduceWindow{}, false)
		})
	})
	if code == 0 || !strings.Contains(errOut, "run `recall sync`") {
		t.Errorf("a hand propose with observations waiting = %d: %s", code, errOut)
	}
	if code, out, errOut := f.sync(t, false); code != 0 {
		t.Fatalf("sync = %d\n%s\n%s", code, out, errOut)
	}
	if waiting(t, f.hookFixture) != 0 {
		t.Error("observations still wait after sync")
	}
	if keys, _ := memRemote.List(context.Background(), "analysis/"); len(keys) == 0 {
		t.Error("the store has no observations after sync")
	}
	if f.pending(t) != 1 {
		t.Error("propose didn't run once the observations were uploaded")
	}
}

func TestSyncStopsProposeWhenTheUploadFails(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	rejectUploads = true
	f := newSyncFixture(t, "rejecttest", "laptop-a", "")
	f.clk.T = monday
	f.transcript(t, "sess", monday.AddDate(0, 0, -2))
	code, out, errOut := f.sync(t, false)
	if code == 0 {
		t.Errorf("sync with a rejected upload = 0\n%s", out)
	}
	if !strings.Contains(errOut, "recall: upload:") || !strings.Contains(out, "propose: not run;") {
		t.Errorf("the upload failure or the stopped propose wasn't reported:\n%s\n%s", out, errOut)
	}
	if exists(f.stamp("learn")) || exists(f.stamp("learn-tried")) || reduces(f.runner.calls) != 0 {
		t.Error("propose ran or was stamped after the upload failed")
	}

	rejectUploads = false
	f.clk.T = monday.Add(time.Hour)
	if code, out, errOut := f.sync(t, false); code != 0 {
		t.Fatalf("sync with the remote back = %d\n%s\n%s", code, out, errOut)
	}
	if waiting(t, f.hookFixture) != 0 || f.pending(t) != 1 {
		t.Error("the next sync didn't upload and propose")
	}
}

// On an fs store with no proposals, sync exits 0 and says one line per step.
func TestSyncFSStoreOneLinePerStep(t *testing.T) {
	f := newSyncFixture(t, "", "laptop-a", "")
	code, out, errOut := f.sync(t, false)
	if code != 0 {
		t.Fatalf("sync = %d\n%s\n%s", code, out, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var steps []string
	for _, l := range lines {
		steps = append(steps, strings.SplitN(l, ":", 2)[0])
	}
	if got := strings.Join(steps, ","); got != "import,archive,observe,propose" {
		t.Errorf("sync's lines are %q:\n%s", got, out)
	}
	if len(f.runner.calls) != 0 {
		t.Errorf("sync with nothing to do made %d model call(s)", len(f.runner.calls))
	}
}

// A store whose reads fail can't say who proposes, so imports and propose
// are skipped, and recorded as skipped, not failed.
func TestSyncSkipsImportsAndProposeWhenTheClaimIsUnreadable(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	remoteDown = true
	f := newSyncFixture(t, "memtest", "laptop-a", syncChat)
	f.clk.T = monday
	f.transcript(t, "sess", monday.AddDate(0, 0, -2))
	_, out, _ := f.sync(t, false)
	if n := len(f.src.fakes["chat"].Specs); n != 0 || reduces(f.runner.calls) != 0 {
		t.Errorf("with the claim unreadable: %d import(s), %d reduce(s)", n, reduces(f.runner.calls))
	}
	for _, want := range []string{"import chat: skipped; couldn't read which machine proposes", "propose: skipped; couldn't read which machine proposes"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync lacks %q:\n%s", want, out)
		}
	}
	st, ok := loadSyncState(config.SyncStatePath(f.home))
	if !ok {
		t.Fatal("no sync state")
	}
	for _, step := range st.Steps {
		if (step.Step == "import chat" || step.Step == "propose") && step.Result != resultSkipped {
			t.Errorf("%s = %s, want skipped", step.Step, step.Result)
		}
	}
	if exists(f.stamp("learn-tried")) {
		t.Error("a propose was tried with the claim unreadable")
	}
}

// The upload needs no model, so a runner that won't open still uploads.
func TestSyncUploadsWithNoRunner(t *testing.T) {
	newSyncStore(t)
	claim(t, "laptop-a")
	f := newSyncFixture(t, "memtest", "laptop-a", "")
	f.clk.T = monday
	f.transcript(t, "sess", monday.AddDate(0, 0, -2))
	observeOnce(t, f.hookFixture)
	if waiting(t, f.hookFixture) == 0 {
		t.Fatal("nothing waits after a hand observe")
	}
	f.env.runner = nil
	f.env.settings.Learn.Runner = "no-such-runner"
	code, out, errOut := f.sync(t, false)
	if code == 0 {
		t.Errorf("sync with no runner = 0\n%s", out)
	}
	if n := waiting(t, f.hookFixture); n != 0 || !strings.Contains(out, "upload: ") {
		t.Errorf("%d objects still wait with no runner:\n%s\n%s", n, out, errOut)
	}
}
