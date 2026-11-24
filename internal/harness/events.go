package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	eventsv1 "github.com/micahhausler/recall/api/events/v1"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/events"
)

// Events reads a harness whose transcripts are recall's own events, one
// session per <id>.jsonl directly in its source, such as an import's
// stored sessions. Its sessions ran on other machines, so it has no
// skills, context files, or memory here.
type Events struct {
	name, source string
}

func openEvents(name string, s config.HarnessSettings) *Events {
	return &Events{name: name, source: s.Source}
}

func (e *Events) Name() string      { return e.name }
func (e *Events) SourceDir() string { return e.source }

func (e *Events) WeekFiles(start, end time.Time) ([]string, error) {
	return weekFiles(e.source, start, end)
}

// Sessions lists the regular *.jsonl files directly in the source.
func (e *Events) Sessions() ([]string, error) {
	entries, err := os.ReadDir(e.source)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range entries {
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".jsonl") {
			out = append(out, filepath.Join(e.source, d.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// SessionID is a session file's name without .jsonl, for a file directly
// in the source, on disk or as the archive names it.
func (e *Events) SessionID(path string) (string, bool) {
	path = filepath.FromSlash(path)
	if !strings.HasSuffix(path, ".jsonl") || filepath.Base(filepath.Dir(path)) != filepath.Base(e.source) {
		return "", false
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl"), true
}

func (e *Events) ReadSessionFile(path, host string) ([]events.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	session, _ := e.SessionID(path)
	return e.ReadSession(f, session, host)
}

// ReadSession reads one session's events. The harness, host, and session
// are recall's to write, whatever the file says. A malformed line, or an
// event that fails eventsv1.Check, is skipped.
func (e *Events) ReadSession(r io.Reader, session, host string) ([]events.Event, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)
	var out []events.Event
	for sc.Scan() {
		var ev events.Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil || eventsv1.Check(ev) != nil {
			continue
		}
		ev.Harness, ev.Host, ev.Session = e.name, host, session
		out = append(out, ev)
	}
	return out, sc.Err()
}

func (e *Events) Inventory(context.Context, string) (Inventory, error) { return Inventory{}, nil }
func (e *Events) ContextFiles() []string                               { return nil }
func (e *Events) Memories() ([]Memory, error)                          { return nil, nil }
