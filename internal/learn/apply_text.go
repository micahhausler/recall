package learn

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/micahhausler/recall/internal/review"
)

// Text mode is how a runner with no Editor applies: its harness can't
// confine write tools to the worktrees, so the model gets no tools at all.
// The input carries the current contents of every file the model may
// change, the model answers with each changed file whole between marker
// lines, and the binary writes only the paths it listed itself.

//go:embed prompts/apply-text.md
var applyTextPrompt string

// defaultMarker starts each begin and end line in text mode; a run whose
// input already holds it uses a numbered variant instead.
const defaultMarker = "%%recall-file%%"

// textFilesBudget caps the bytes of editable file contents one text-mode
// input carries; a proposal whose files would go past it is held back.
const textFilesBudget = 256 << 10

// textContextLimit caps one read-only file, a worktree's AGENTS.md or
// CLAUDE.md, shown for its conventions.
const textContextLimit = 32 << 10

// textMode reports whether the apply runs with no tools and writes the
// model's answer itself.
func (a *Applier) textMode() bool {
	return a.Editor == nil && a.Runner != nil
}

// textPaths are the live files a route may change in text mode: its script
// and the files it edits, less any settings file, which only the binary
// edits.
func (a *Applier) textPaths(r Route) []string {
	settings := ""
	if a.Settings != "" {
		settings = review.Resolve(a.Settings)
	}
	var out []string
	for _, p := range append([]string{r.Script}, r.Edit...) {
		if p != "" && p != r.Settings && p != settings {
			out = append(out, p)
		}
	}
	return out
}

// textBudget returns a run's check of each route's files against
// textFilesBudget, in route order, counting each file once.
func textBudget() func(paths []string) string {
	seen := map[string]bool{}
	used := int64(0)
	return func(paths []string) string {
		n := int64(0)
		var fresh []string
		for _, p := range paths {
			if seen[p] {
				continue
			}
			fresh = append(fresh, p)
			if st, err := os.Stat(p); err == nil {
				n += st.Size()
			}
		}
		if used+n > textFilesBudget {
			return fmt.Sprintf("the runner has no editor, so the apply input carries every file it changes, and its files would bring that to %d bytes, over the %d-byte limit",
				used+n, textFilesBudget)
		}
		used += n
		for _, p := range fresh {
			seen[p] = true
		}
		return ""
	}
}

// textEntry is one file in a text-mode input's FILES section.
type textEntry struct {
	path string
	// routed is a file a proposal changes; editable is one the binary
	// accepts back. A routed file that fails the checks is neither shown
	// nor editable.
	routed, editable bool
	// content is nil for a file that doesn't exist or isn't shown.
	content []byte
	// note says why an entry isn't shown or isn't editable.
	note string
}

// textFiles is a text-mode input's FILES section, and what the binary
// accepts back.
type textFiles struct {
	marker  string
	entries []textEntry
	// editable maps each path the model may return to its worktree.
	editable map[string]string
}

// textFiles collects the routes' editable files and each worktree's own
// AGENTS.md or CLAUDE.md. Before the worktrees exist, for a dry run, the
// live checkout stands in for each.
func (a *Applier) textFiles(plan *Plan, m mapping) *textFiles {
	tf := &textFiles{editable: map[string]string{}}
	seen := map[string]bool{}
	add := func(at string, w *review.Worktree, editable bool) {
		at = filepath.Clean(at)
		if seen[at] {
			return
		}
		seen[at] = true
		root, src := w.Dir, at
		if _, err := os.Stat(w.Dir); err != nil {
			rel, _ := filepath.Rel(w.Dir, at)
			root, src = w.Repo, filepath.Join(w.Repo, rel)
		}
		e := textEntry{path: at, routed: editable, editable: editable}
		check := confinedRead
		if editable {
			check = confinedWrite
		}
		if err := check(src, root); err != nil {
			e.editable, e.note = false, err.Error()
		} else if data, err := os.ReadFile(src); err == nil {
			e.content = data
			if !editable && len(data) > textContextLimit {
				e.content, e.note = nil, fmt.Sprintf("%d bytes, too large to show", len(data))
			}
		} else if !os.IsNotExist(err) {
			e.editable, e.note = false, err.Error()
		} else if !editable {
			return
		}
		if e.editable {
			tf.editable[at] = w.Dir
		}
		tf.entries = append(tf.entries, e)
	}
	for _, r := range plan.Routes {
		if r.Skip != "" {
			continue
		}
		for _, live := range a.textPaths(r) {
			if at, w := m.at(live); w != nil {
				add(at, w, true)
			}
		}
	}
	for _, w := range m.wts {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			add(filepath.Join(w.Dir, name), w, false)
		}
	}
	return tf
}

// pickMarker sets a marker no line of the rest of the input or of any file
// already holds, so the answer's begin and end lines are unambiguous.
func (tf *textFiles) pickMarker(rest string) {
	taken := func(marker string) bool {
		if strings.Contains(rest, marker) {
			return true
		}
		for _, e := range tf.entries {
			if bytes.Contains(e.content, []byte(marker)) {
				return true
			}
		}
		return false
	}
	tf.marker = defaultMarker
	for i := 2; taken(tf.marker); i++ {
		tf.marker = "%%recall-file-" + strconv.Itoa(i) + "%%"
	}
}

func (tf *textFiles) render(b *strings.Builder) {
	b.WriteString("\n# FILES\n\n")
	var editable, readOnly, refused []string
	for _, e := range tf.entries {
		switch {
		case e.editable && e.content == nil:
			editable = append(editable, "- "+e.path+" (doesn't exist yet; return it whole to create it)")
		case e.editable:
			editable = append(editable, "- "+e.path)
		case e.routed:
			refused = append(refused, "- "+e.path+": "+e.note)
		case e.note != "":
			readOnly = append(readOnly, "- "+e.path+" (not shown: "+e.note+")")
		default:
			readOnly = append(readOnly, "- "+e.path)
		}
	}
	for _, sec := range []struct {
		head  string
		lines []string
	}{
		{"Editable: return any of these whole to change it.", editable},
		{"Read only: follow these, never return them.", readOnly},
		{"Not editable: the binary won't write these, so skip a proposal that needs one.", refused},
	} {
		if len(sec.lines) > 0 {
			b.WriteString(sec.head + "\n" + strings.Join(sec.lines, "\n") + "\n\n")
		}
	}
	for _, e := range tf.entries {
		if e.content == nil {
			continue
		}
		fmt.Fprintf(b, "%s BEGIN %s\n", tf.marker, e.path)
		b.Write(e.content)
		if len(e.content) > 0 && !bytes.HasSuffix(e.content, []byte("\n")) {
			b.WriteString("\n")
		}
		fmt.Fprintf(b, "%s END %s\n", tf.marker, e.path)
	}
}

// textAnswerFile is one file a text-mode answer returns.
type textAnswerFile struct {
	path string
	data []byte
}

// parseTextAnswer splits a text-mode answer into the files it returns and
// the text outside them, where the outcome lines are. An unterminated
// block, or a marker line out of place, makes the answer malformed.
func parseTextAnswer(out, marker string) ([]textAnswerFile, string, error) {
	var files []textAnswerFile
	var rest, body []string
	var cur *textAnswerFile
	for i, line := range strings.Split(out, "\n") {
		directive, ok := strings.CutPrefix(strings.TrimSpace(line), marker+" ")
		if !ok {
			if cur != nil {
				body = append(body, line)
			} else {
				rest = append(rest, line)
			}
			continue
		}
		verb, path, _ := strings.Cut(directive, " ")
		path = strings.TrimSpace(path)
		switch {
		case cur == nil && verb == "BEGIN" && path != "":
			cur, body = &textAnswerFile{path: path}, nil
		case cur != nil && verb == "END" && path == cur.path:
			data := strings.Join(body, "\n")
			if len(body) > 0 {
				data += "\n"
			}
			cur.data = []byte(data)
			files = append(files, *cur)
			cur = nil
		default:
			return nil, "", fmt.Errorf("malformed answer: line %d, %q, is out of place", i+1, strings.TrimSpace(line))
		}
	}
	if cur != nil {
		return nil, "", fmt.Errorf("malformed answer: the block for %s has no end line", cur.path)
	}
	return files, strings.Join(rest, "\n"), nil
}

// refuse says why the binary won't write path, if it won't. n is how many
// times the answer returns it.
func (tf *textFiles) refuse(path string, n int) string {
	if !filepath.IsAbs(path) {
		return "it isn't an absolute path FILES lists as editable"
	}
	if n > 1 {
		return "the answer returns it more than once"
	}
	wt, ok := tf.editable[path]
	if !ok {
		return "it isn't a file FILES lists as editable"
	}
	if !within(path, wt) {
		return "it is outside its worktree"
	}
	if err := confinedWrite(path, wt); err != nil {
		return err.Error()
	}
	return ""
}

// writeAnswer writes the files a text-mode answer returns and gives back
// its outcomes. A refused file is reported and not written, and a proposal
// that names one is not applied; its other files are withheld too, unless
// an applied proposal also names them.
func (a *Applier) writeAnswer(out string, tf *textFiles) ([]Outcome, error) {
	files, rest, err := parseTextAnswer(out, tf.marker)
	if err != nil {
		return nil, err
	}
	outcomes := ParseOutcomes(rest)
	if len(outcomes) == 0 {
		return nil, errors.New("malformed answer: it has no Applied or Skipped lines")
	}
	count := map[string]int{}
	for _, f := range files {
		count[filepath.Clean(f.path)]++
	}
	refused := map[string]string{}
	for _, f := range files {
		if p := filepath.Clean(f.path); refused[p] == "" {
			if why := tf.refuse(p, count[p]); why != "" {
				refused[p] = why
			}
		}
	}
	named := func(o Outcome) []string {
		var why []string
		for _, p := range o.Paths {
			if r := refused[filepath.Clean(p)]; r != "" {
				why = append(why, filepath.Clean(p)+" ("+r+")")
			}
		}
		return why
	}
	keep, drop := map[string]bool{}, map[string]bool{}
	for _, o := range outcomes {
		if o.Applied && len(named(o)) == 0 {
			for _, p := range o.Paths {
				keep[filepath.Clean(p)] = true
			}
		}
	}
	for i := range outcomes {
		o := &outcomes[i]
		why := named(*o)
		if !o.Applied || len(why) == 0 {
			continue
		}
		for _, p := range o.Paths {
			drop[filepath.Clean(p)] = true
		}
		o.Applied, o.Paths = false, nil
		o.Detail = "the binary refused to write " + strings.Join(why, "; ")
	}
	for _, f := range files {
		p := filepath.Clean(f.path)
		if why := refused[p]; why != "" {
			fmt.Fprintf(a.out(), "recall: apply: refused to write %s: %s\n", f.path, why)
			continue
		}
		if drop[p] && !keep[p] {
			fmt.Fprintf(a.out(), "recall: apply: not writing %s: the proposal that changes it named a refused file\n", p)
			continue
		}
		if err := writeConfined(p, tf.editable[p], f.data); err != nil {
			return nil, fmt.Errorf("writing %s: %w", p, err)
		}
	}
	return outcomes, nil
}

// confinedWrite says why the binary won't write path inside root, if it
// won't: path itself is a symlink or not a regular file, or the deepest
// part of it that exists resolves outside root.
func confinedWrite(path, root string) error {
	if !within(path, root) {
		return fmt.Errorf("it is outside %s", root)
	}
	fi, err := os.Lstat(path)
	switch {
	case err == nil && fi.Mode()&os.ModeSymlink != 0:
		return errors.New("it is a symlink, which the binary won't write through")
	case err == nil && !fi.Mode().IsRegular():
		return errors.New("it isn't a regular file")
	case err != nil && !os.IsNotExist(err):
		return err
	}
	dir := filepath.Dir(path)
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		dir = filepath.Dir(dir)
	}
	return resolvesWithin(dir, root)
}

// confinedRead says why the binary won't read path for the input, if it
// won't: it resolves outside root. A symlink within root is fine to read.
func confinedRead(path, root string) error {
	if !within(path, root) {
		return fmt.Errorf("it is outside %s", root)
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	return resolvesWithin(path, root)
}

func resolvesWithin(path, root string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("%s doesn't resolve: %w", path, err)
	}
	if !within(real, realRoot) {
		return fmt.Errorf("%s resolves to %s, outside the worktree", path, real)
	}
	return nil
}

// writeConfined writes data at path, checking it again first. An existing
// file keeps its mode, since os.WriteFile sets one only on create.
func writeConfined(path, root string, data []byte) error {
	if err := confinedWrite(path, root); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
