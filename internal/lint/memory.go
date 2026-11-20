package lint

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Note is one note a harness's own memory wrote, as the lint needs it.
type Note struct {
	// Path is the note's file, and Index the index that should list it;
	// empty when the harness keeps none.
	Path, Index string
	Modified    time.Time
}

var indexLinkRE = regexp.MustCompile(`\]\(([^)\s]+\.md)\)`)

// Memory lints a harness's native memory with the rules context files get,
// and three of its own: a note the index doesn't list is one the harness
// never loads, an index line that points at a missing note is a dead
// pointer in every session, and a note nobody has changed in staleDays has
// had nothing keep it current. It reads and reports; it never edits a note.
func (l *Linter) Memory(notes []Note, staleDays int, now time.Time) ([]Finding, error) {
	var files []string
	byIndex := map[string][]Note{}
	var indexes []string
	for _, n := range notes {
		files = append(files, n.Path)
		if n.Index != "" {
			if byIndex[n.Index] == nil {
				indexes = append(indexes, n.Index)
			}
			byIndex[n.Index] = append(byIndex[n.Index], n)
		}
	}
	findings, err := l.Run(files)
	if err != nil {
		return nil, err
	}
	sort.Strings(indexes)
	for _, index := range indexes {
		listed := map[string]bool{}
		f, err := os.Open(index)
		if err == nil {
			sc := bufio.NewScanner(f)
			for n := 1; sc.Scan(); n++ {
				for _, m := range indexLinkRE.FindAllStringSubmatch(sc.Text(), -1) {
					target := filepath.Join(filepath.Dir(index), m[1])
					listed[target] = true
					if _, err := os.Stat(target); err != nil {
						findings = append(findings, Finding{index, n, fmt.Sprintf("index points at %s, which doesn't exist", m[1])})
					}
				}
			}
			f.Close()
		}
		for _, n := range byIndex[index] {
			if !listed[n.Path] {
				findings = append(findings, Finding{n.Path, 1, fmt.Sprintf("not listed in %s, so the harness never loads it", filepath.Base(index))})
			}
		}
	}
	if staleDays > 0 {
		for _, n := range notes {
			if days := int(now.Sub(n.Modified).Hours() / 24); days > staleDays {
				findings = append(findings, Finding{n.Path, 1, fmt.Sprintf("last changed %d days ago; nothing has kept it current", days)})
			}
		}
	}
	return findings, nil
}
