package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/micahhausler/recall/internal/clock"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/store"
)

// tree builds a fake ~/.claude/projects with files at chosen mtimes.
type tree struct {
	t      *testing.T
	source string
}

func newTree(t *testing.T) *tree {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, ".claude", "projects")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	return &tree{t, source}
}

func (tr *tree) touch(rel string, when time.Time, body string) {
	tr.t.Helper()
	path := filepath.Join(tr.source, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *tree) harness(t *testing.T) harness.Harness {
	h, err := harness.Open("claude", config.HarnessSettings{Source: tr.source, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestWeekFilesSelectsByMtimeHalfOpen(t *testing.T) {
	tr := newTree(t)
	start, end := Week{2026, 45}.Bounds(time.Local)
	sec := time.Second
	tr.touch("p/at-start.jsonl", start, "x")
	tr.touch("p/sub/agents/mid.jsonl", start.AddDate(0, 0, 3), "x")
	tr.touch("p/before.jsonl", start.Add(-sec), "x")
	tr.touch("p/at-end.jsonl", end, "x")
	tr.touch("q/last-second.jsonl", end.Add(-sec), "x")
	os.Symlink("at-start.jsonl", filepath.Join(tr.source, "p/link.jsonl"))
	got, err := tr.harness(t).WeekFiles(start, end)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"projects/p/at-start.jsonl", "projects/p/sub/agents/mid.jsonl", "projects/q/last-second.jsonl"}
	if !equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
}

func TestWeekFilesNoSourceDir(t *testing.T) {
	tr := newTree(t)
	os.RemoveAll(tr.source)
	start, end := Week{2026, 45}.Bounds(time.Local)
	got, err := tr.harness(t).WeekFiles(start, end)
	if err != nil || len(got) != 0 {
		t.Errorf("files = %v, %v; want none", got, err)
	}
}

func TestBuildRoundTrip(t *testing.T) {
	tr := newTree(t)
	start, _ := Week{2026, 45}.Bounds(time.Local)
	body := strings.Repeat(`{"type":"user","message":"hello"}`+"\n", 1000)
	tr.touch("-home-you/a b.jsonl", start, body)
	tr.touch("-home-you/s/sub.jsonl", start, "sub\n")
	h := tr.harness(t)
	files, err := h.WeekFiles(start, start.AddDate(0, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.tar.zst")
	kept, size, err := BuildFile(filepath.Dir(tr.source), files, out)
	if err != nil || len(kept) != 2 {
		t.Fatalf("build: %v, kept %v", err, kept)
	}
	if size >= int64(len(body))/10 {
		t.Errorf("compressed %d bytes, want < a tenth of %d", size, len(body))
	}

	// Plain `zstd -d` must read it: the layout is byte-compatible with
	// archives that already exist.
	var raw []byte
	if _, err := exec.LookPath("zstd"); err == nil {
		cmd := exec.Command("zstd", "-d", "-c", out)
		raw, err = cmd.Output()
		if err != nil {
			t.Fatalf("zstd -d: %v", err)
		}
	} else {
		t.Log("zstd not installed; decoding in-process only")
		data, _ := os.ReadFile(out)
		dec, _ := zstd.NewReader(bytes.NewReader(data))
		raw, err = io.ReadAll(dec)
		if err != nil {
			t.Fatal(err)
		}
	}
	tt := tar.NewReader(bytes.NewReader(raw))
	got := map[string]string{}
	for {
		hdr, err := tt.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tt)
		got[hdr.Name] = string(data)
	}
	if got["projects/-home-you/a b.jsonl"] != body || got["projects/-home-you/s/sub.jsonl"] != "sub\n" {
		t.Errorf("round trip mismatch; names: %v", keysOf(got))
	}
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBuildDropsDeletedFiles(t *testing.T) {
	tr := newTree(t)
	start, _ := Week{2026, 45}.Bounds(time.Local)
	tr.touch("p/stays.jsonl", start, "x")
	out := filepath.Join(t.TempDir(), "out.tar.zst")
	kept, _, err := BuildFile(filepath.Dir(tr.source), []string{"projects/p/stays.jsonl", "projects/p/gone.jsonl"}, out)
	if err != nil || !equal(kept, []string{"projects/p/stays.jsonl"}) {
		t.Errorf("kept = %v, %v", kept, err)
	}

	kept, _, err = BuildFile(filepath.Dir(tr.source), []string{"projects/gone.jsonl"}, out+"2")
	if err != nil || len(kept) != 0 {
		t.Errorf("all-deleted: kept = %v, %v", kept, err)
	}
	if _, err := os.Stat(out + "2"); !os.IsNotExist(err) {
		t.Error("an output file was written with nothing to archive")
	}
}

// runWeek is Monday 2026-11-10 (W46); lookback 4 covers W45..W42.
var today = time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)

func runner(t *testing.T, tr *tree, st store.Store) (*Runner, *strings.Builder, *strings.Builder) {
	out, errOut := &strings.Builder{}, &strings.Builder{}
	return &Runner{
		Store:     st,
		Harnesses: []harness.Harness{tr.harness(t)},
		Host:      "box",
		Clock:     &clock.Fake{T: today},
		Loc:       time.Local,
		Dest:      func(key string) string { return "s3://bkt/" + key },
		Out:       out,
		Err:       errOut,
	}, out, errOut
}

func touchWeek(tr *tree, w Week, name string) {
	start, _ := w.Bounds(time.Local)
	tr.touch(name, start.Add(time.Hour), "data-"+name)
}

func TestRunUploadsOnlyMissingNonemptyWeeks(t *testing.T) {
	tr := newTree(t)
	touchWeek(tr, Week{2026, 45}, "p/a.jsonl")
	touchWeek(tr, Week{2026, 45}, "p/b.jsonl")
	touchWeek(tr, Week{2026, 44}, "p/c.jsonl")
	touchWeek(tr, Week{2026, 43}, "p/d.jsonl")
	st := store.NewMem()
	ctx := context.Background()
	if err := st.PutIfAbsent(ctx, "claude/box/2026/W44.tar.zst", strings.NewReader("already")); err != nil {
		t.Fatal(err)
	}
	r, out, _ := runner(t, tr, st)
	failed, err := r.Run(ctx, 4, false, false)
	if err != nil || failed != 0 {
		t.Fatalf("failed=%d err=%v", failed, err)
	}
	keys, _ := st.List(ctx, "claude/box/")
	want := []string{"claude/box/2026/W43.tar.zst", "claude/box/2026/W44.tar.zst", "claude/box/2026/W45.tar.zst"}
	if !equal(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
	if got, _ := st.Get(ctx, "claude/box/2026/W44.tar.zst"); got != nil {
		data, _ := io.ReadAll(got)
		if string(data) != "already" {
			t.Error("an existing week was overwritten")
		}
	}
	for _, wantLine := range []string{
		"2026-W45: uploaded 2 files",
		"2026-W42: no changed files, nothing to upload",
	} {
		if !strings.Contains(out.String(), wantLine) {
			t.Errorf("output misses %q:\n%s", wantLine, out)
		}
	}
}

func TestRunExistingObjectRace(t *testing.T) {
	// The week is missing at List time but exists at Put time: not a failure.
	tr := newTree(t)
	touchWeek(tr, Week{2026, 45}, "p/a.jsonl")
	st := store.NewMem()
	ctx := context.Background()
	race := &racingStore{Mem: st}
	r, out, _ := runner(t, tr, race)
	failed, err := r.Run(ctx, 1, false, false)
	if err != nil || failed != 0 {
		t.Fatalf("failed=%d err=%v", failed, err)
	}
	if !strings.Contains(out.String(), "2026-W45: already in") {
		t.Errorf("output = %s", out)
	}
}

// racingStore reports no keys but refuses every put as existing.
type racingStore struct{ *store.Mem }

func (r *racingStore) List(ctx context.Context, prefix string) ([]string, error) {
	return nil, nil
}

func (r *racingStore) PutIfAbsent(ctx context.Context, key string, rd io.Reader) error {
	return store.ErrExists
}

type failingStore struct{ *store.Mem }

func (f *failingStore) PutIfAbsent(ctx context.Context, key string, rd io.Reader) error {
	return io.ErrClosedPipe
}

func TestRunUploadFailureExits1AndContinues(t *testing.T) {
	tr := newTree(t)
	touchWeek(tr, Week{2026, 45}, "p/a.jsonl")
	touchWeek(tr, Week{2026, 44}, "p/b.jsonl")
	r, _, errOut := runner(t, tr, &failingStore{store.NewMem()})
	failed, err := r.Run(context.Background(), 4, false, false)
	if err != nil || failed != 2 {
		t.Fatalf("failed=%d err=%v", failed, err)
	}
	if !strings.Contains(errOut.String(), "upload failed") {
		t.Errorf("stderr = %s", errOut)
	}
}

func TestRunDryRunAndOffline(t *testing.T) {
	tr := newTree(t)
	touchWeek(tr, Week{2026, 45}, "p/a.jsonl")
	st := store.NewMem()
	ctx := context.Background()
	r, out, _ := runner(t, tr, st)
	failed, err := r.Run(ctx, 4, true, false)
	if err != nil || failed != 0 {
		t.Fatal(err)
	}
	if keys, _ := st.List(ctx, ""); len(keys) != 0 {
		t.Errorf("dry run uploaded: %v", keys)
	}
	if !strings.Contains(out.String(), "would upload 1 files") || !strings.Contains(out.String(), "s3://bkt/claude/box/2026/W45.tar.zst") {
		t.Errorf("output = %s", out)
	}

	// Offline never lists the store.
	r2, out2, _ := runner(t, tr, &listBomb{})
	r2.Dest = func(key string) string { return "s3://<bucket>/" + key }
	if _, err := r2.Run(ctx, 4, true, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.String(), "s3://<bucket>/claude/box/2026/W45") {
		t.Errorf("offline output = %s", out2)
	}
}

// listBomb fails the test if the store is touched.
type listBomb struct{ store.Mem }

func (l *listBomb) List(ctx context.Context, prefix string) ([]string, error) {
	panic("offline run listed the store")
}

func TestRunLookback(t *testing.T) {
	tr := newTree(t)
	for w := 30; w <= 45; w++ {
		touchWeek(tr, Week{2026, w}, strings.Repeat("f", 1)+Week{2026, w}.String()+".jsonl")
	}
	st := store.NewMem()
	r, _, _ := runner(t, tr, st)
	if _, err := r.Run(context.Background(), 8, false, false); err != nil {
		t.Fatal(err)
	}
	keys, _ := st.List(context.Background(), "")
	if len(keys) != 8 {
		t.Errorf("uploaded %d weeks, want 8: %v", len(keys), keys)
	}
}
