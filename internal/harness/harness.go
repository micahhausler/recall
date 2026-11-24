// Package harness adapts one coding agent's chat history: where it keeps
// transcripts, what its archive prefix is, and how a session becomes
// normalized events. Backends register under the name config uses, so
// adding an agent is a new file, not a change to callers.
package harness

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/events"
)

// Harness is one coding agent's chat history.
type Harness interface {
	// Name is the archive prefix ("claude"), so each agent's uploads land
	// beside the others'.
	Name() string
	// SourceDir is where the harness keeps transcripts on this machine.
	SourceDir() string
	// WeekFiles returns the paths a week's archive includes: every file under
	// SourceDir modified in [start, end), relative to SourceDir's parent, so
	// a tarball unpacks to recognizable paths like projects/....
	WeekFiles(start, end time.Time) ([]string, error)
	// Sessions lists this harness's transcript files on disk.
	Sessions() ([]string, error)
	// SessionID names the session a transcript path holds, on disk or inside
	// a week's archive; ok is false for a path that isn't a transcript.
	SessionID(path string) (id string, ok bool)
	// ReadSessionFile reads one transcript from disk into normalized events.
	ReadSessionFile(path, host string) ([]events.Event, error)
	// ReadSession parses one transcript stream, named session, into events.
	ReadSession(r io.Reader, session, host string) ([]events.Event, error)
	// Inventory lists the skills and slash commands the harness offers to a
	// session started in dir, as the harness itself reports them.
	Inventory(ctx context.Context, dir string) (Inventory, error)
	// ContextFiles lists the global context files this harness loads into
	// every session (CLAUDE.md, AGENTS.md, steering) that exist on disk.
	ContextFiles() []string
	// Memories lists the notes the harness's own memory has written on this
	// machine. A harness with no memory of its own returns none.
	Memories() ([]Memory, error)
}

// Memory is one note a harness's built-in memory wrote: something the agent
// decided to remember, kept on the machine that wrote it and reviewed by
// nobody. recall reads these as evidence and never writes them.
type Memory struct {
	// Path is the note's file, and Index the index file that should list
	// it, when the harness keeps one.
	Path, Index string
	// Project is the harness's name for the project the note belongs to,
	// and CWD the directory that project stands for, when a transcript
	// says.
	Project, CWD string
	// Name, Description, and Type come from the note's frontmatter; Type
	// is the harness's own kind, like feedback or project.
	Name, Description, Type string
	// Session is the session the note came from, when the note records it.
	Session string
	// Modified is when the note last changed.
	Modified time.Time
	// Body is the note's text after its frontmatter.
	Body string
}

// SessionMetadata recovers what a harness's transcripts don't carry from the
// metadata file archived beside them. A harness whose transcripts are
// self-contained doesn't implement it, and an archive written without the
// metadata just yields what the transcripts hold.
type SessionMetadata interface {
	// MetadataSession names the session a metadata path describes; ok is
	// false for a path that isn't session metadata.
	MetadataSession(path string) (id string, ok bool)
	// ReadSessionMetadata parses one metadata stream into the session's
	// working directory, empty when it doesn't say.
	ReadSessionMetadata(r io.Reader) string
}

// Factory opens a harness from its merged settings.
type Factory func(s config.HarnessSettings) (Harness, error)

var registry = map[string]Factory{}

// Register adds a harness under its config name.
func Register(name string, f Factory) {
	registry[name] = f
}

// Open opens one named harness. A name recall doesn't know opens the
// adapter its settings' Format names, under that name; the events format
// is recall's own.
func Open(name string, s config.HarnessSettings) (Harness, error) {
	if f, ok := registry[name]; ok {
		return f(s)
	}
	if s.Format == "" {
		return nil, fmt.Errorf("unknown harness %q; set harness.%s.format to the known harness whose transcript format it shares", name, name)
	}
	if s.Format == config.FormatEvents {
		return openEvents(name, s), nil
	}
	f, ok := registry[s.Format]
	if !ok {
		return nil, fmt.Errorf("harness %q: unknown format %q", name, s.Format)
	}
	h, err := f(s)
	if err != nil {
		return nil, err
	}
	return &formatted{Harness: h, name: name}, nil
}

// Enabled opens every enabled harness in the settings, sorted by name.
func Enabled(s *config.Settings) ([]Harness, error) {
	names := make([]string, 0, len(s.Harnesses))
	for n := range s.Harnesses {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []Harness
	for _, n := range names {
		h, err := Open(n, s.Harnesses[n])
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// WalkFiles returns (path relative to root's parent, info) for every regular
// file under root, sorted; symlinks are skipped. A missing root is empty.
type FileInfo struct {
	Rel     string
	Size    int64
	ModTime time.Time
}

func WalkFiles(root string) ([]FileInfo, error) {
	base := filepath.Dir(root)
	var out []FileInfo
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return filepath.SkipAll
			}
			return nil // a file deleted mid-walk is not an error
		}
		if !d.Type().IsRegular() {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		out = append(out, FileInfo{Rel: filepath.ToSlash(rel), Size: st.Size(), ModTime: st.ModTime()})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, nil
}

// weekFiles is the shared mtime selection every directory-based harness uses.
func weekFiles(root string, start, end time.Time) ([]string, error) {
	files, err := WalkFiles(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if !f.ModTime.Before(start) && f.ModTime.Before(end) {
			out = append(out, f.Rel)
		}
	}
	return out, nil
}

// ReadTarZst reads every transcript inside a week's .tar.zst archive into events,
// so analysis can run over archived weeks the same way it runs over disk.
func ReadTarZst(h Harness, path, host string) ([]events.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	evs, err := ReadTarZstFrom(h, f, host)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return evs, nil
}

// ReadTarZstFrom reads a week's archive from a stream, such as a store
// object, into events. A harness whose transcripts lack a working directory
// (SessionMetadata) gets each session's filled from the metadata file
// archived beside it, when the archive has one.
func ReadTarZstFrom(h Harness, r io.Reader, host string) ([]events.Event, error) {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	tr := tar.NewReader(dec)
	md, _ := h.(SessionMetadata)
	cwds := map[string]string{}
	var out []events.Event
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if md != nil {
			if session, ok := md.MetadataSession(hdr.Name); ok {
				if cwd := md.ReadSessionMetadata(tr); cwd != "" {
					cwds[session] = cwd
				}
				continue
			}
		}
		session, ok := h.SessionID(hdr.Name)
		if !ok {
			continue
		}
		evs, err := h.ReadSession(tr, session, host)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", hdr.Name, err)
		}
		out = append(out, evs...)
	}
	// The metadata may come after its transcript in the tar, so fill at the end.
	for i := range out {
		if out[i].CWD == "" {
			out[i].CWD = cwds[out[i].Session]
		}
	}
	return out, nil
}
