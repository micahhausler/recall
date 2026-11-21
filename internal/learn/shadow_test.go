package learn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The model edits copies of a worktree's .claude directory; what it changes
// or creates there lands back in the worktree, a path outside every root is
// left alone, and the model's output names the worktree paths again.
func TestShadowClaudeDirsRoundTrip(t *testing.T) {
	wt := filepath.Join(t.TempDir(), "wt")
	run := t.TempDir()
	dot := filepath.Join(wt, ".claude")
	if err := os.MkdirAll(filepath.Join(dot, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dot, "CLAUDE.md"), []byte("- old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := "/elsewhere/.claude/CLAUDE.md"
	input := "- edit: " + filepath.Join(dot, "CLAUDE.md") + "\n" +
		"- a new skill goes at " + filepath.Join(dot, "skills") + "/<name>/SKILL.md\n" +
		"- edit: " + outside + "\n"

	got, root, shadows, err := shadowClaudeDirs(input, run, []string{wt})
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(run, shadowDir) || len(shadows) != 1 {
		t.Fatalf("root %q, shadows %+v", root, shadows)
	}
	if strings.Contains(got, dot) {
		t.Errorf("input still names the worktree's .claude:\n%s", got)
	}
	if !strings.Contains(got, outside) {
		t.Errorf("a path outside every root was rewritten:\n%s", got)
	}
	cp := shadows[0].copy
	if b, err := os.ReadFile(filepath.Join(cp, "CLAUDE.md")); err != nil || string(b) != "- old\n" {
		t.Fatalf("shadow copy = %q, %v", b, err)
	}

	// The model edits one file and creates a skill in the copy.
	if err := os.WriteFile(filepath.Join(cp, "CLAUDE.md"), []byte("- new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(cp, "skills", "practice", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("steps\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syncBack(shadows); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dot, "CLAUDE.md")); string(b) != "- new\n" {
		t.Errorf("edit not synced back: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dot, "skills", "practice", "SKILL.md")); string(b) != "steps\n" {
		t.Errorf("new skill not synced back: %q", b)
	}
	if o := unshadow("Applied[add]: x -- "+filepath.Join(cp, "CLAUDE.md"), shadows); o != "Applied[add]: x -- "+filepath.Join(dot, "CLAUDE.md") {
		t.Errorf("unshadow = %q", o)
	}
}

// A symlink under the .claude directory isn't copied out, and a file the
// model creates at its path is refused on the way back, not written
// through the link to wherever it points.
func TestSyncBackRefusesSymlinks(t *testing.T) {
	wt := filepath.Join(t.TempDir(), "wt")
	run := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("untouched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dot := filepath.Join(wt, ".claude")
	if err := os.MkdirAll(dot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dot, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(dot, "skills")); err != nil {
		t.Fatal(err)
	}
	input := "- edit: " + filepath.Join(dot, "CLAUDE.md") + "\n"
	_, _, shadows, err := shadowClaudeDirs(input, run, []string{wt})
	if err != nil {
		t.Fatal(err)
	}
	cp := shadows[0].copy
	for _, rel := range []string{"CLAUDE.md", filepath.Join("skills", "x", "SKILL.md")} {
		p := filepath.Join(cp, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("written\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := syncBack(shadows); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("syncBack = %v, want a symlink refusal", err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "untouched\n" {
		t.Errorf("wrote through the file symlink: %q", b)
	}
	if entries, _ := os.ReadDir(outsideDir); len(entries) != 0 {
		t.Errorf("wrote through the directory symlink: %v", entries)
	}
}
