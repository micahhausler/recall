// Package skills reports which skills sessions actually invoke, straight
// from the "rules are earned" tenet: a skill nobody's sessions use is a
// retirement candidate.
package skills

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/micahhausler/recall/internal/events"
)

// Skill is one skill found on disk.
type Skill struct {
	Name string
	Path string
}

// Discover finds skills as <dir>/*/SKILL.md under each given directory,
// reading the name from frontmatter and falling back to the directory name.
func Discover(dirs []string) ([]Skill, error) {
	var out []Skill
	seen := map[string]bool{}
	for _, dir := range dirs {
		matches, err := filepath.Glob(filepath.Join(dir, "*", "SKILL.md"))
		if err != nil {
			return nil, err
		}
		sort.Strings(matches)
		for _, m := range matches {
			name := frontmatterName(m)
			if name == "" {
				name = filepath.Base(filepath.Dir(m))
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, Skill{Name: name, Path: m})
		}
	}
	return out, nil
}

func frontmatterName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return ""
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "---" {
			return ""
		}
		if rest, ok := strings.CutPrefix(line, "name:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// Row is one skill's usage over the window.
type Row struct {
	Name     string `json:"name"`
	Path     string `json:"path,omitempty"`
	ByModel  int    `json:"by_model"`
	BySlash  int    `json:"by_slash"`
	Sessions int    `json:"sessions"`
	LastUsed string `json:"last_used,omitempty"`
	Flag     string `json:"flag,omitempty"`
}

// Flags a report sets.
const (
	FlagRetire    = "never invoked in the window; retirement candidate"
	FlagSlashOnly = "only ever invoked by slash command; description may not match how you ask"
	FlagNotOnDisk = "invoked but not found on this machine"
)

// Report aggregates skill invocations from an event stream over the skills
// found on disk. Skills invoked but not found are reported too.
func Report(found []Skill, evs []events.Event) []Row {
	rows := map[string]*Row{}
	order := []string{}
	add := func(name string) *Row {
		r, ok := rows[name]
		if !ok {
			r = &Row{Name: name}
			rows[name] = r
			order = append(order, name)
		}
		return r
	}
	for _, s := range found {
		r := add(s.Name)
		r.Path = s.Path
	}
	sessions := map[string]map[string]bool{}
	for _, e := range evs {
		var name string
		switch e.Kind {
		case events.SkillInvoked:
			name = e.Skill
		case events.SlashCommand:
			name = e.Skill
		default:
			continue
		}
		if name == "" {
			continue
		}
		r := add(name)
		if e.Kind == events.SkillInvoked {
			r.ByModel++
		} else {
			r.BySlash++
		}
		if e.Timestamp > r.LastUsed {
			r.LastUsed = e.Timestamp
		}
		if sessions[name] == nil {
			sessions[name] = map[string]bool{}
		}
		sessions[name][e.Session] = true
	}
	var out []Row
	for _, name := range order {
		r := rows[name]
		r.Sessions = len(sessions[name])
		switch {
		case r.Path == "":
			r.Flag = FlagNotOnDisk
		case r.ByModel == 0 && r.BySlash == 0:
			r.Flag = FlagRetire
		case r.ByModel == 0:
			r.Flag = FlagSlashOnly
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].ByModel+out[i].BySlash, out[j].ByModel+out[j].BySlash; a != b {
			return a > b
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Print writes a readable table.
func Print(w io.Writer, rows []Row, since time.Time) {
	fmt.Fprintf(w, "%-24s %8s %8s %8s  %-20s %s\n", "SKILL", "MODEL", "SLASH", "SESSIONS", "LAST USED", "FLAG")
	for _, r := range rows {
		last := r.LastUsed
		if t, err := time.Parse(time.RFC3339, r.LastUsed); err == nil {
			last = t.Format("2006-01-02")
		}
		if last == "" {
			last = "-"
		}
		fmt.Fprintf(w, "%-24s %8d %8d %8d  %-20s %s\n", r.Name, r.ByModel, r.BySlash, r.Sessions, last, r.Flag)
	}
}
