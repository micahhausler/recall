package harness

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/config"
)

func init() {
	Register("claude", func(s config.HarnessSettings) (Harness, error) {
		return &Claude{source: s.Source}, nil
	})
}

// Claude reads Claude Code's chat history: JSONL transcripts under
// ~/.claude/projects, one directory per project, one file per session.
type Claude struct {
	source string
}

func (c *Claude) Name() string      { return "claude" }
func (c *Claude) SourceDir() string { return c.source }

func (c *Claude) WeekFiles(start, end time.Time) ([]string, error) {
	return weekFiles(c.source, start, end)
}

// SessionID is a transcript's file name without .jsonl.
func (c *Claude) SessionID(path string) (string, bool) {
	if !strings.HasSuffix(path, ".jsonl") {
		return "", false
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl"), true
}

// ContextFiles is CLAUDE.md beside the projects directory, ~/.claude by
// default; it pulls in the rest through @imports.
func (c *Claude) ContextFiles() []string {
	p := filepath.Join(filepath.Dir(c.source), "CLAUDE.md")
	if _, err := os.Stat(p); err != nil {
		return nil
	}
	return []string{p}
}
