package harness

import (
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
