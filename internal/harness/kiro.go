package harness

import (
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/events"
)

func init() {
	Register("kiro", func(s config.HarnessSettings) (Harness, error) {
		return &Kiro{source: s.Source}, nil
	})
}

// Kiro reads Kiro CLI's chat history under ~/.kiro/sessions, in both of the
// layouts its engines write:
//
//   - V2: cli/<id>.jsonl, with metadata (cwd, created_at) in cli/<id>.json.
//   - v3: <workspace hash>/sess_<id>/messages.jsonl, with metadata in
//     session.json beside it.
type Kiro struct {
	source string
}

func (k *Kiro) Name() string      { return "kiro" }
func (k *Kiro) SourceDir() string { return k.source }

// root is the Kiro home, ~/.kiro by default: the sessions directory's parent.
func (k *Kiro) root() string { return filepath.Dir(k.source) }

// WeekFiles is every file under SourceDir modified in the week, less the
// lock files Kiro holds while a session is open.
func (k *Kiro) WeekFiles(start, end time.Time) ([]string, error) {
	files, err := weekFiles(k.source, start, end)
	if err != nil {
		return nil, err
	}
	out := files[:0]
	for _, f := range files {
		if !strings.HasSuffix(f, ".lock") {
			out = append(out, f)
		}
	}
	return out, nil
}

// SessionID is <id> for a V2 cli/<id>.jsonl, and sess_<id> for a v3
// sess_<id>/messages.jsonl.
func (k *Kiro) SessionID(p string) (string, bool) {
	p = filepath.ToSlash(p)
	dir, base := path.Base(path.Dir(p)), path.Base(p)
	switch {
	case base == "messages.jsonl" && strings.HasPrefix(dir, "sess_"):
		return dir, true
	case dir == "cli" && strings.HasSuffix(base, ".jsonl"):
		return strings.TrimSuffix(base, ".jsonl"), true
	}
	return "", false
}

func (k *Kiro) Sessions() ([]string, error) {
	files, err := WalkFiles(k.source)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if _, ok := k.SessionID(f.Rel); ok {
			out = append(out, filepath.Join(filepath.Dir(k.source), filepath.FromSlash(f.Rel)))
		}
	}
	return out, nil
}

// ContextFiles is every Markdown file under ~/.kiro/steering, AGENTS.md
// included; Kiro loads them all into every session.
func (k *Kiro) ContextFiles() []string {
	dir := filepath.Join(k.root(), "steering")
	files, err := WalkFiles(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f.Rel, ".md") {
			out = append(out, filepath.Join(k.root(), filepath.FromSlash(f.Rel)))
		}
	}
	return out
}

// Memories returns none: Kiro has no built-in memory store of its own;
// steering files are owner-reviewed context, not notes the agent wrote.
func (k *Kiro) Memories() ([]Memory, error) { return nil, nil }

// ReadSessionFile reads one transcript from disk, taking the working
// directory from the session's metadata file, which the transcript lacks.
func (k *Kiro) ReadSessionFile(p, host string) ([]events.Event, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	session, _ := k.SessionID(p)
	evs, err := k.ReadSession(f, session, host)
	if err != nil {
		return nil, err
	}
	if cwd := k.cwd(p); cwd != "" {
		for i := range evs {
			if evs[i].CWD == "" {
				evs[i].CWD = cwd
			}
		}
	}
	return evs, nil
}

// MetadataSession names the session a metadata path describes: <id> for a V2
// cli/<id>.json, sess_<id> for a v3 sess_<id>/session.json. The metadata
// lives under SourceDir, so a week's archive carries it beside the
// transcripts, and an archive read recovers the cwd the transcripts lack.
func (k *Kiro) MetadataSession(p string) (string, bool) {
	p = filepath.ToSlash(p)
	dir, base := path.Base(path.Dir(p)), path.Base(p)
	switch {
	case base == "session.json" && strings.HasPrefix(dir, "sess_"):
		return dir, true
	case dir == "cli" && strings.HasSuffix(base, ".json"):
		return strings.TrimSuffix(base, ".json"), true
	}
	return "", false
}

// ReadSessionMetadata reads a session's working directory from its metadata
// stream: cwd in V2, the first workspace path in v3. Metadata that is
// missing, unreadable, or silent is just an empty cwd.
func (k *Kiro) ReadSessionMetadata(r io.Reader) string {
	var meta struct {
		CWD            string   `json:"cwd"`
		WorkspacePaths []string `json:"workspacePaths"`
	}
	if json.NewDecoder(r).Decode(&meta) != nil {
		return ""
	}
	if meta.CWD != "" {
		return meta.CWD
	}
	if len(meta.WorkspacePaths) > 0 {
		return meta.WorkspacePaths[0]
	}
	return ""
}

// cwd reads a session's working directory from its metadata file on disk.
func (k *Kiro) cwd(transcript string) string {
	meta := strings.TrimSuffix(transcript, ".jsonl") + ".json"
	if filepath.Base(transcript) == "messages.jsonl" {
		meta = filepath.Join(filepath.Dir(transcript), "session.json")
	}
	f, err := os.Open(meta)
	if err != nil {
		return ""
	}
	defer f.Close()
	return k.ReadSessionMetadata(f)
}
