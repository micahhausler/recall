// Package claude reads Claude Code's transcripts and native memory.
package claude

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/harness"
)

// New opens Claude Code's chat history: JSONL transcripts under
// ~/.claude/projects, one directory per project, one file per session.
func New(s config.HarnessSettings) (harness.Harness, error) {
	return &adapter{source: s.Source}, nil
}

type adapter struct {
	source string
}

func (c *adapter) Name() string      { return config.HarnessClaude }
func (c *adapter) SourceDir() string { return c.source }

func (c *adapter) WeekFiles(start, end time.Time) ([]string, error) {
	return harness.WeekFiles(c.source, start, end)
}

// SessionID is a transcript's file name without .jsonl.
func (c *adapter) SessionID(path string) (string, bool) {
	if !strings.HasSuffix(path, ".jsonl") {
		return "", false
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl"), true
}

// ContextFiles is CLAUDE.md beside the projects directory, ~/.claude by
// default; it pulls in the rest through @imports.
func (c *adapter) ContextFiles() []string {
	p := filepath.Join(filepath.Dir(c.source), "CLAUDE.md")
	if _, err := os.Stat(p); err != nil {
		return nil
	}
	return []string{p}
}
