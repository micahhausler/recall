// Package harness adapts one coding agent's chat history: where it keeps
// transcripts, what its archive prefix is, and (in the claude adapter) how a
// session becomes normalized events. Backends register under the name config
// uses, so adding an agent is a new file, not a change to callers.
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
	"strings"
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
	// ReadSessionFile reads one transcript from disk into normalized events.
	ReadSessionFile(path, host string) ([]events.Event, error)
	// ReadSession parses one transcript stream, named session, into events.
	ReadSession(r io.Reader, session, host string) ([]events.Event, error)
	// Inventory lists the skills and slash commands the harness offers to a
	// session started in dir, as the harness itself reports them.
	Inventory(ctx context.Context, dir string) (Inventory, error)
}

// Factory opens a harness from its merged settings.
type Factory func(s config.HarnessSettings) (Harness, error)

var registry = map[string]Factory{}

// Register adds a harness under its config name.
func Register(name string, f Factory) {
	registry[name] = f
}

// Open opens one named harness.
func Open(name string, s config.HarnessSettings) (Harness, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q", name)
	}
	return f(s)
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

// ReadTarZst reads every .jsonl inside a week's .tar.zst archive into events,
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
// object, into events.
func ReadTarZstFrom(h Harness, r io.Reader, host string) ([]events.Event, error) {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	tr := tar.NewReader(dec)
	var out []events.Event
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(hdr.Name, ".jsonl") {
			continue
		}
		session := strings.TrimSuffix(filepath.Base(hdr.Name), ".jsonl")
		evs, err := h.ReadSession(tr, session, host)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", hdr.Name, err)
		}
		out = append(out, evs...)
	}
	return out, nil
}
