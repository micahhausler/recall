package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/micahhausler/recall/internal/store"
)

// fakeToken has a GitHub token's shape; it's assembled at run time so the
// repo holds nothing a secret scanner would flag.
var fakeToken = "gh" + "p_" + strings.Repeat("a1B2", 9)

// members decodes a tar.zst into name -> content.
func members(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	dec, err := zstd.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	tr := tar.NewReader(dec)
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(data)) != hdr.Size {
			t.Fatalf("%s: %d bytes, header says %d", hdr.Name, len(data), hdr.Size)
		}
		got[hdr.Name] = string(data)
	}
}

func TestBuildRedactsTextMembers(t *testing.T) {
	tr := newTree(t)
	start, _ := Week{2026, 45}.Bounds(time.Local)
	line, err := json.Marshal(map[string]string{"type": "user", "text": "push with\n" + fakeToken + "\nplease"})
	if err != nil {
		t.Fatal(err)
	}
	transcript := string(line) + "\n" + `{"type":"assistant","text":"done"}` + "\n"
	tr.touch("p/s.jsonl", start, transcript)
	tr.touch("p/s/tool-results/out.txt", start, "token: "+fakeToken+"\n")
	tr.touch("p/clean.jsonl", start, `{"type":"user","text":"hello"}`+"\n")
	tr.touch("p/image.png", start, "\x89PNG "+fakeToken)
	files := []string{"projects/p/clean.jsonl", "projects/p/image.png", "projects/p/s.jsonl", "projects/p/s/tool-results/out.txt"}

	var plain, redacted bytes.Buffer
	if _, err := Build(filepath.Dir(tr.source), files, &plain, Options{}); err != nil {
		t.Fatal(err)
	}
	built, err := Build(filepath.Dir(tr.source), files, &redacted, Options{Redact: true})
	if err != nil {
		t.Fatal(err)
	}
	if built.Redactions != 2 || len(built.Files) != 4 {
		t.Errorf("redactions = %d, files = %v; want 2 redactions in 4 files", built.Redactions, built.Files)
	}

	// Off, the archive holds the files byte for byte.
	was := members(t, &plain)
	if was["projects/p/s.jsonl"] != transcript {
		t.Error("with redaction off, a transcript changed")
	}

	got := members(t, &redacted)
	for name, body := range got {
		if name != "projects/p/image.png" && strings.Contains(body, fakeToken) {
			t.Errorf("%s still holds the token", name)
		}
	}
	if got["projects/p/image.png"] != was["projects/p/image.png"] {
		t.Error("a file that isn't text was rewritten")
	}
	if got["projects/p/clean.jsonl"] != was["projects/p/clean.jsonl"] {
		t.Error("a transcript with nothing to redact changed")
	}
	if got["projects/p/s/tool-results/out.txt"] != "token: [REDACTED:github-token]\n" {
		t.Errorf("tool result = %q", got["projects/p/s/tool-results/out.txt"])
	}
	lines := strings.Split(strings.TrimSuffix(got["projects/p/s.jsonl"], "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("redacted transcript has %d lines, want 2", len(lines))
	}
	var rec map[string]string
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("redacted line is no longer JSON: %v", err)
	}
	if rec["text"] != "push with\n[REDACTED:github-token]\nplease" {
		t.Errorf("text = %q", rec["text"])
	}
}

func TestRunReportsRedactions(t *testing.T) {
	tr := newTree(t)
	start, _ := Week{2026, 45}.Bounds(time.Local)
	tr.touch("p/a.jsonl", start.Add(time.Hour), `{"text":"`+fakeToken+`"}`+"\n")
	st := store.NewMem()
	ctx := context.Background()

	r, out, _ := runner(t, tr, st)
	r.Build = Options{Redact: true}
	if failed, err := r.Run(ctx, 1, true, false); err != nil || failed != 0 {
		t.Fatalf("dry run: failed=%d err=%v", failed, err)
	}
	if !strings.Contains(out.String(), "would upload 1 files") || !strings.Contains(out.String(), "; redacted 1 secret-shaped token(s)") {
		t.Errorf("dry run output = %s", out)
	}
	out.Reset()
	if failed, err := r.Run(ctx, 1, false, false); err != nil || failed != 0 {
		t.Fatalf("run: failed=%d err=%v", failed, err)
	}
	if !strings.Contains(out.String(), "; redacted 1 secret-shaped token(s)") {
		t.Errorf("run output = %s", out)
	}
	obj, err := st.Get(ctx, "claude/box/2026/W45.tar.zst")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()
	if body := members(t, obj)["projects/p/a.jsonl"]; body != `{"text":"[REDACTED:github-token]"}`+"\n" {
		t.Errorf("uploaded transcript = %q", body)
	}
	// The file on disk is never touched.
	disk, _ := os.ReadFile(filepath.Join(tr.source, "p/a.jsonl"))
	if !strings.Contains(string(disk), fakeToken) {
		t.Error("redaction changed the transcript on disk")
	}

	// Off is the default, and says nothing about redaction.
	plain, out2, _ := runner(t, tr, store.NewMem())
	if _, err := plain.Run(ctx, 1, true, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2.String(), "redacted") {
		t.Errorf("output with redaction off = %s", out2)
	}
}
