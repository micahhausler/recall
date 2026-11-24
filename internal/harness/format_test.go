package harness_test

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/harness"
)

// A harness recall doesn't know reads through the adapter its format names
// and goes by its own name, on disk and in an archive.
func TestFormattedHarness(t *testing.T) {
	root := filepath.Join(t.TempDir(), "other-config")
	source := filepath.Join(root, "projects")
	plain, _ := os.ReadFile(filepath.Join("testdata", "plain.jsonl"))
	put(t, filepath.Join(source, "-work-a", "abc.jsonl"), string(plain))
	put(t, filepath.Join(root, "CLAUDE.md"), "- a rule\n")
	h, err := harness.Open("work", config.HarnessSettings{Source: source, Enabled: true, Format: config.HarnessClaude})
	if err != nil {
		t.Fatal(err)
	}
	if h.Name() != "work" || h.SourceDir() != source {
		t.Errorf("name %q, source %q", h.Name(), h.SourceDir())
	}
	sessions, err := h.Sessions()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions = %v, %v", sessions, err)
	}
	evs, err := h.ReadSessionFile(sessions[0], "laptop-a")
	if err != nil || len(evs) == 0 {
		t.Fatalf("events = %v, %v", evs, err)
	}
	for _, e := range evs {
		if e.Harness != "work" {
			t.Errorf("event harness = %q", e.Harness)
		}
	}
	if got := h.ContextFiles(); !reflect.DeepEqual(got, []string{filepath.Join(root, "CLAUDE.md")}) {
		t.Errorf("context files = %v", got)
	}
}

// A Kiro-format harness still recovers each archived session's cwd from the
// metadata beside it.
func TestFormattedHarnessKeepsMetadata(t *testing.T) {
	v2, _ := os.ReadFile(filepath.Join("testdata", "kiro", "v2.jsonl"))
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range []struct{ name, body string }{
		{"sessions/cli/abc.json", `{"cwd":"/work/a"}`},
		{"sessions/cli/abc.jsonl", string(v2)},
	} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	zw.Close()
	h, err := harness.Open("kiro-b", config.HarnessSettings{Source: t.TempDir(), Enabled: true, Format: config.HarnessKiro})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := harness.ReadTarZstFrom(h, &buf, "laptop-b")
	if err != nil || len(evs) == 0 {
		t.Fatalf("events = %v, %v", evs, err)
	}
	for _, e := range evs {
		if e.CWD != "/work/a" || e.Harness != "kiro-b" {
			t.Errorf("event cwd %q, harness %q", e.CWD, e.Harness)
		}
	}
}

func TestOpenUnknownHarness(t *testing.T) {
	for _, tc := range []struct {
		format, want string
	}{
		{"", "set harness.work.format"},
		{"other", `unknown format "other"`},
	} {
		_, err := harness.Open("work", config.HarnessSettings{Source: t.TempDir(), Format: tc.format})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("format %q: err = %v, want %q", tc.format, err, tc.want)
		}
	}
}
