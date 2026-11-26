package claude_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMemories(t *testing.T) {
	source := filepath.Join(t.TempDir(), "projects")
	project := filepath.Join(source, "-home-you-src-project")
	mem := filepath.Join(project, "memory")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "-home-you-empty", "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put(filepath.Join(project, "s1.jsonl"), `{"type":"summary"}`+"\n"+`{"type":"user","cwd":"/home/you/src/project","message":{"role":"user","content":"hi"}}`+"\n")
	put(filepath.Join(mem, "MEMORY.md"), "- [Ready PRs](ready-prs.md) — open ready, not draft\n")
	put(filepath.Join(mem, "ready-prs.md"), "---\nname: ready-prs\ndescription: \"Open PRs \\\"ready\\\", not as drafts\"\nmetadata:\n  node_type: memory\n"+
		"  type: feedback\n  originSessionId: s1\n  modified: 2026-11-08T07:09:53.401Z\n---\n\nOpen PRs as regular PRs.\n\n**Why:** the owner said so.\n")
	put(filepath.Join(mem, "bare.md"), "A note with no frontmatter.\n")

	mems, err := open(t, source).Memories()
	if err != nil || len(mems) != 2 {
		t.Fatalf("memories = %+v, %v", mems, err)
	}
	bare, ready := mems[0], mems[1]
	if ready.Name != "ready-prs" || ready.Description != `Open PRs "ready", not as drafts` || ready.Type != "feedback" ||
		ready.Session != "s1" || ready.Project != "-home-you-src-project" || ready.CWD != "/home/you/src/project" ||
		ready.Index != filepath.Join(mem, "MEMORY.md") || ready.Body != "Open PRs as regular PRs.\n\n**Why:** the owner said so." {
		t.Errorf("note with frontmatter = %+v", ready)
	}
	if !ready.Modified.Equal(time.Date(2026, 11, 8, 7, 9, 53, 401000000, time.UTC)) {
		t.Errorf("modified = %v, want the frontmatter's time", ready.Modified)
	}
	if bare.Name != "bare" || bare.Type != "" || bare.Body != "A note with no frontmatter." || bare.Modified.IsZero() {
		t.Errorf("bare note = %+v", bare)
	}
	if none, err := open(t, filepath.Join(t.TempDir(), "missing")).Memories(); err != nil || len(none) != 0 {
		t.Errorf("no transcript root: %+v, %v", none, err)
	}
}
