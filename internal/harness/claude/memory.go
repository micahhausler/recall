package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/harness"
)

// memoryIndex is the file Claude Code's auto memory lists its notes in.
const memoryIndex = "MEMORY.md"

// Memories reads Claude Code's auto memory: one directory per project under
// the transcript root, <project>/memory/, holding an index and one Markdown
// note per memory with YAML frontmatter.
func (c *adapter) Memories() ([]harness.Memory, error) {
	projects, err := os.ReadDir(c.source)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []harness.Memory
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(c.source, p.Name(), "memory")
		notes, err := os.ReadDir(dir)
		if err != nil {
			continue // most projects have no memory
		}
		cwd := ""
		looked := false
		for _, n := range notes {
			if n.IsDir() || !strings.HasSuffix(n.Name(), ".md") || n.Name() == memoryIndex {
				continue
			}
			if !looked {
				cwd, looked = projectCWD(filepath.Join(c.source, p.Name())), true
			}
			path := filepath.Join(dir, n.Name())
			m, err := readMemory(path)
			if err != nil {
				continue // a note deleted mid-read
			}
			m.Index = filepath.Join(dir, memoryIndex)
			m.Project, m.CWD = p.Name(), cwd
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// readMemory parses one note: frontmatter between --- lines, then the body.
// The frontmatter is read line by line for the few keys recall uses, at any
// nesting, since the harness has moved them between the top level and a
// metadata block.
func readMemory(path string) (harness.Memory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return harness.Memory{}, err
	}
	m := harness.Memory{Path: path, Name: strings.TrimSuffix(filepath.Base(path), ".md")}
	if st, err := os.Stat(path); err == nil {
		m.Modified = st.ModTime()
	}
	text := string(data)
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		if front, body, ok := strings.Cut(rest, "\n---"); ok {
			text = strings.TrimLeft(strings.TrimPrefix(body, "\n"), "\n")
			for _, line := range strings.Split(front, "\n") {
				key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
				if !ok {
					continue
				}
				value = strings.TrimSpace(value)
				// A double-quoted YAML scalar escapes like a JSON string.
				if unquoted, err := strconv.Unquote(value); err == nil && strings.HasPrefix(value, `"`) {
					value = unquoted
				} else {
					value = strings.Trim(value, `"'`)
				}
				switch strings.TrimSpace(key) {
				case "name":
					m.Name = value
				case "description":
					m.Description = value
				case "type":
					m.Type = value
				case "originSessionId":
					m.Session = value
				case "modified":
					if t, err := time.Parse(time.RFC3339, value); err == nil {
						m.Modified = t
					}
				}
			}
		}
	}
	m.Body = strings.TrimSpace(text)
	return m, nil
}

// projectCWD is the working directory a project's transcripts ran in, read
// from the first transcript line that names one. The project directory's
// own name encodes the path with its separators replaced, which can't be
// reversed, so a transcript is the only record.
func projectCWD(dir string) string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	sort.Strings(files)
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)
		cwd := ""
		for i := 0; i < 50 && sc.Scan() && cwd == ""; i++ {
			var l struct {
				CWD string `json:"cwd"`
			}
			if json.Unmarshal(sc.Bytes(), &l) == nil {
				cwd = l.CWD
			}
		}
		f.Close()
		if cwd != "" {
			return cwd
		}
	}
	return ""
}
