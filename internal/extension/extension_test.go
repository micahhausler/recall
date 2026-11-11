package extension

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.md")
	b := filepath.Join(dir, "b.md")
	blank := filepath.Join(dir, "blank.md")
	os.WriteFile(a, []byte("- first\n\n"), 0o644)
	os.WriteFile(b, []byte("- second"), 0o644)
	os.WriteFile(blank, []byte("\n  \n"), 0o644)
	var warn bytes.Buffer
	got := Read([]string{a, filepath.Join(dir, "missing.md"), blank, b}, &warn)
	if got != "- first\n\n- second\n" {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(warn.String(), "missing.md") {
		t.Errorf("warn = %q, want the missing file named", warn.String())
	}
	if Read(nil, nil) != "" {
		t.Error("no files should read as empty")
	}
}
