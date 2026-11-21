package learn

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/micahhausler/recall/internal/review"
)

// Repo is one checkout whose own context files proposals may land in
// (learn.repos). A lesson every session of which ran in the repo belongs in
// the repo's file, where only that repo's sessions pay for it.
type Repo struct {
	// Root is the checkout, symlinks resolved; Remote is its origin as
	// host/path, empty without one.
	Root   string
	Remote string
	Files  []RepoFile
}

// RepoFile is one context file in a repo.
type RepoFile struct {
	Path string
	// Globs are the paths a path-scoped rules file applies to; empty for a
	// file the harness loads for the whole repo or directory.
	Globs []string
	// New marks a file that doesn't exist yet: the repo has no context
	// file, so this is where its first rule would go.
	New bool
}

// rootFiles are the context files a harness loads for a whole repo, in the
// order a harness prefers them.
var rootFiles = []string{"CLAUDE.md", filepath.Join(".claude", "CLAUDE.md"), "AGENTS.md"}

// nestedDepth is how far below the root a directory's own CLAUDE.md or
// AGENTS.md is looked for.
const nestedDepth = 3

// DiscoverRepo finds a checkout's tracked context files: the root files a
// harness loads at launch, path-scoped rules under .claude/rules, and a
// directory's own CLAUDE.md or AGENTS.md a few levels down. Only files git
// tracks count: review stages edits in a worktree, where an untracked or
// ignored file doesn't exist, and a checkout at ~ would otherwise pull in
// every context file under the home directory. Two names for one file,
// such as a symlink, count once. A repo with no root file offers a new
// CLAUDE.md, unless an untracked file already sits there.
func DiscoverRepo(root string) (Repo, error) {
	root = review.Resolve(root)
	repo := Repo{Root: root}
	if _, remote := LocateCheckout("")(root); remote != "" {
		repo.Remote = remote
	}
	tracked, err := trackedFiles(root)
	if err != nil {
		return repo, err
	}
	seen := map[string]bool{}
	add := func(path string, globs []string) {
		real := review.Resolve(path)
		if seen[real] {
			return
		}
		seen[real] = true
		repo.Files = append(repo.Files, RepoFile{Path: real, Globs: globs})
	}
	isTracked := map[string]bool{}
	for _, rel := range tracked {
		isTracked[rel] = true
	}
	for _, name := range rootFiles {
		path := filepath.Join(root, name)
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && isTracked[name] {
			add(path, nil)
		}
	}
	if len(repo.Files) == 0 {
		path := filepath.Join(root, rootFiles[0])
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			repo.Files = append(repo.Files, RepoFile{Path: path, New: true})
		}
	}

	rules := filepath.Join(".claude", "rules") + string(filepath.Separator)
	var scoped, nested []string
	for _, rel := range tracked {
		path := filepath.Join(root, rel)
		if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() {
			continue
		}
		switch {
		case strings.HasPrefix(rel, rules) && strings.HasSuffix(rel, ".md"):
			scoped = append(scoped, path)
		case nestedContextFile(rel):
			nested = append(nested, path)
		}
	}
	sort.Strings(scoped)
	for _, path := range scoped {
		add(path, ruleGlobs(path))
	}
	sort.Strings(nested)
	for _, path := range nested {
		add(path, nil)
	}
	return repo, nil
}

// nestedContextFile says whether rel, a tracked path, is a directory's own
// CLAUDE.md or AGENTS.md below the root: no deeper than nestedDepth
// directories, and outside hidden, node_modules, and vendor directories.
func nestedContextFile(rel string) bool {
	name := filepath.Base(rel)
	if name != "CLAUDE.md" && name != "AGENTS.md" {
		return false
	}
	dirs := strings.Split(filepath.Dir(rel), string(filepath.Separator))
	if dirs[0] == "." || len(dirs) > nestedDepth {
		return false
	}
	for _, d := range dirs {
		if strings.HasPrefix(d, ".") || d == "node_modules" || d == "vendor" {
			return false
		}
	}
	return true
}

// trackedFiles lists the files git tracks in the checkout at root, relative
// to it, in the platform's separator.
func trackedFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errOut.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("git ls-files: %s", detail)
	}
	var files []string
	for _, rel := range strings.Split(out.String(), "\x00") {
		if rel != "" {
			files = append(files, filepath.FromSlash(rel))
		}
	}
	return files, nil
}

// ruleGlobs reads the paths a rules file's frontmatter scopes it to: an
// inline list or a block list under "paths:". A file with no frontmatter, or
// none the harness would read, applies everywhere and returns nothing.
func ruleGlobs(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil
	}
	var globs []string
	inPaths := false
	clean := func(s string) string { return strings.Trim(strings.TrimSpace(s), `"'`) }
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			break
		}
		if rest, ok := strings.CutPrefix(trimmed, "paths:"); ok {
			inPaths = true
			rest = strings.TrimSpace(rest)
			rest = strings.TrimSuffix(strings.TrimPrefix(rest, "["), "]")
			for _, g := range strings.Split(rest, ",") {
				if g = clean(g); g != "" {
					globs = append(globs, g)
				}
			}
			continue
		}
		if item, ok := strings.CutPrefix(trimmed, "- "); ok && inPaths {
			if g := clean(item); g != "" {
				globs = append(globs, g)
			}
			continue
		}
		if trimmed != "" {
			inPaths = false
		}
	}
	return globs
}

// file returns the repo's context file at path, if it has one.
func (r Repo) file(path string) *RepoFile {
	for i := range r.Files {
		if r.Files[i].Path == path {
			return &r.Files[i]
		}
	}
	return nil
}

// holds reports whether a session's repo key, a Scope.Key, names this repo:
// the same origin, or the same path on this machine.
func (r Repo) holds(key, home string) bool {
	if key == "" {
		return false
	}
	if r.Remote != "" && key == r.Remote {
		return true
	}
	return review.Resolve(ExpandScopePath(key, home)) == r.Root
}

// Paths lists the repo's existing context files, for the rule ledger.
func (r Repo) Paths() []string {
	var out []string
	for _, f := range r.Files {
		if !f.New {
			out = append(out, f.Path)
		}
	}
	return out
}

// Scoping says which sessions a target's rules apply to: a repo's file to
// the sessions that ran in that repo, and any other target to all of them.
type Scoping struct {
	Repos []Repo
	Home  string
}

// repoOf returns the repo whose context file path is, or nil for a global
// target.
func repoOf(repos []Repo, path string) *Repo {
	for i := range repos {
		if repos[i].file(path) != nil {
			return &repos[i]
		}
	}
	return nil
}

// Applies reports whether a rule in target applies to a session with scope
// sc.
func (s *Scoping) Applies(target string, sc Scope) bool {
	if s == nil {
		return true
	}
	repo := repoOf(s.Repos, target)
	if repo == nil {
		return true
	}
	return repo.holds(sc.Key(), s.Home)
}

// ScopeProblem is a new rule aimed at a repo's file on evidence from
// somewhere else.
type ScopeProblem struct {
	Title, Why string
}

// scopeRepo is the listed repo a proposal's placement is scoped to, and the
// file that scopes it there, or nil for a global placement: the target
// itself when it is a repo's context file, and for a graduate, the context
// file of the ledger rule it replaces, which is also where its enforcement
// goes.
func scopeRepo(p Proposal, target string, repos []Repo, rules []Rule) (*Repo, string) {
	switch p.Kind {
	case "add", "reword":
		return repoOf(repos, target), target
	case "skill":
		// A skill is scoped by the file it changes; a new skill with only a
		// name goes to learn.skills, which no repo scopes.
		if strings.TrimSpace(p.Target) == "" {
			return nil, ""
		}
		for i := range repos {
			if within(target, repos[i].Root) {
				return &repos[i], target
			}
		}
	case "graduate":
		for i := range rules {
			if rules[i].ID == p.Replaces {
				at := review.Resolve(rules[i].Target)
				return repoOf(repos, at), at
			}
		}
	}
	return nil, ""
}

// scopeProblem says why a proposal can't go to its repo-scoped place, if it
// can't: a rule in a repo's own file, or enforcement in a repo's own
// settings, must rest only on sessions that ran in that repo, by the reduce
// run's evidence record.
func scopeProblem(p Proposal, target string, repos []Repo, e RunEvidence, home string, rules []Rule) string {
	repo, at := scopeRepo(p, target, repos, rules)
	if repo == nil {
		return ""
	}
	target = at
	refs := e.sessions(p.Candidates)
	if len(refs) == 0 {
		return fmt.Sprintf("it targets %s but cites no sessions the binary can place in that repo", target)
	}
	for _, ref := range refs {
		if !repo.holds(ref.Repo, home) {
			where := ref.Repo
			if where == "" {
				where = "outside any repo"
			}
			return fmt.Sprintf("it targets %s but cites session %s, which ran in %s", target, shortSession(ref.Session), where)
		}
	}
	return ""
}

// narrowScope names the one listed repo all of a global add's evidence comes
// from, if there is one: the rule would hold in that repo's own file and
// cost nothing elsewhere.
func narrowScope(p Proposal, target string, repos []Repo, e RunEvidence, home string) *Repo {
	if p.Kind != "add" || repoOf(repos, target) != nil {
		return nil
	}
	refs := e.sessions(p.Candidates)
	if len(refs) == 0 {
		return nil
	}
	for i := range repos {
		all := true
		for _, ref := range refs {
			all = all && repos[i].holds(ref.Repo, home)
		}
		if all {
			return &repos[i]
		}
	}
	return nil
}
