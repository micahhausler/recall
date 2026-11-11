// Package harness adapts one coding agent's chat history: where it keeps
// transcripts, what its archive prefix is, and (in the claude adapter) how a
// session becomes normalized events. Backends register under the name config
// uses, so adding an agent is a new file, not a change to callers.
package harness

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/micahhausler/recall/internal/config"
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
