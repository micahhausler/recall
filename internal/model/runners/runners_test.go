package runners

import (
	"errors"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/model"
)

func TestOpen(t *testing.T) {
	for _, name := range []string{"claude", "kiro"} {
		if _, err := Open(name); err != nil {
			t.Errorf("Open(%q): %v", name, err)
		}
	}
	if _, err := Open("nope"); err == nil || !strings.Contains(err.Error(), "claude, kiro") {
		t.Errorf("Open(nope) err = %v", err)
	}
}

// Kiro can't confine edits to directories, so it has no editor, and apply
// runs it with no tools instead of editing through another harness.
func TestOpenEditor(t *testing.T) {
	if _, err := OpenEditor("claude"); err != nil {
		t.Errorf("OpenEditor(claude): %v", err)
	}
	if _, err := OpenEditor("kiro"); !errors.Is(err, model.ErrNoEditor) {
		t.Errorf("OpenEditor(kiro) err = %v", err)
	}
	if _, err := OpenEditor("nope"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("OpenEditor(nope) err = %v", err)
	}
}
