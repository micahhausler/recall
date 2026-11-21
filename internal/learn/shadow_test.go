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
