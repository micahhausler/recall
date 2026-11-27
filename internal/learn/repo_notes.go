package learn

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// A repo note is the owner's file of rules for one repo recall doesn't
// change (learn.repo_notes): it lives in an owner repo, keyed by the other
// repo's origin, and a harness's session-start hook loads it only in that
// repo's sessions (Gates.NoteHarnesses). As a Repo, a repo note has no Root, its Remote is the repo
// it is for, and its one file is the repo note.

// RepoNotePath is the repo note file for the repo whose origin is remote, as
// host/path, under dir; empty when remote isn't a key a path can hold.
func RepoNotePath(dir, remote string) string {
	if dir == "" || !repoNoteKey(remote) {
		return ""
	}
	return filepath.Join(dir, filepath.FromSlash(remote)+".md")
}

// repoNoteKey reports whether remote is a host/path origin whose every
// segment is a plain name and whose host has a dot.
func repoNoteKey(remote string) bool {
	parts := strings.Split(remote, "/")
	if len(parts) < 2 || strings.ContainsAny(remote, `\:`) {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.HasPrefix(p, ".") || strings.HasPrefix(p, "~") {
			return false
		}
	}
	return strings.Contains(parts[0], ".")
}

// repoNoteRemote is the origin a repo note file under dir is for, or "" when
// path isn't one.
func repoNoteRemote(dir, path string) string {
	if dir == "" {
		return ""
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil || !strings.HasSuffix(rel, ".md") {
		return ""
	}
	remote := filepath.ToSlash(strings.TrimSuffix(rel, ".md"))
	if !repoNoteKey(remote) {
		return ""
	}
	return remote
}

// repoNoteRepo is the repo note for remote under dir, marked New when the file
// doesn't exist yet.
func repoNoteRepo(dir, remote string) Repo {
	path := RepoNotePath(dir, remote)
	_, err := os.Lstat(path)
	return Repo{Remote: remote, RepoNote: true, Files: []RepoFile{{Path: path, New: errors.Is(err, fs.ErrNotExist)}}}
}

// DiscoverRepoNotes lists the repo note files under dir, sorted by repo. A
// missing dir has none.
func DiscoverRepoNotes(dir string) ([]Repo, error) {
	if dir == "" {
		return nil, nil
	}
	var out []Repo
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if remote := repoNoteRemote(dir, path); remote != "" {
			out = append(out, Repo{Remote: remote, RepoNote: true, Files: []RepoFile{{Path: path}}})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Remote < out[j].Remote })
	return out, err
}

// addRepoNotes adds a repo note for each repo origin in remotes that has no
// listed repo or repo note yet, so the reduce can offer it and the apply can
// stage it. Nothing is added without learn.repo_notes.
func (g *Gates) addRepoNotes(remotes []string) {
	if g.RepoNotes == "" {
		return
	}
	for _, remote := range remotes {
		if RepoNotePath(g.RepoNotes, remote) == "" {
			continue
		}
		known := false
		for _, r := range g.Repos {
			known = known || r.Remote == remote
		}
		if !known {
			g.Repos = append(g.Repos[:len(g.Repos):len(g.Repos)], repoNoteRepo(g.RepoNotes, remote))
		}
	}
}

// addProposedRepoNotes adds the repo note each proposal targets, so the
// reduce and the apply hold the same Repos whatever the reduce offered.
func (g *Gates) addProposedRepoNotes(props []Proposal, norm func(string) string) {
	var remotes []string
	for _, prop := range props {
		if remote := repoNoteRemote(g.RepoNotes, norm(prop.Target)); remote != "" {
			remotes = append(remotes, remote)
		}
	}
	g.addRepoNotes(remotes)
}

// publicNote says why a repo note path can't take a change, if it can't:
// it resolves under a keep.public path.
func publicNote(path string, public []string, resolve func(string) string) string {
	path = resolve(path)
	for _, pub := range public {
		if pub = resolve(pub); path == pub || strings.HasPrefix(path, pub+string(filepath.Separator)) {
			return fmt.Sprintf("%s is under keep.public %s; repo notes describe other people's repos and stay private", path, pub)
		}
	}
	return ""
}

// repoNoteRemotes are the origins of the repos outside learn.repos that open
// candidates rest on wholly, with a session from a harness in loaders among
// them: the repos a new repo note is offered for.
func repoNoteRemotes(open []Cluster, loaders []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range open {
		if len(k.Repos) != 1 || k.Placed != k.Sessions || seen[k.Repos[0]] || !repoNoteKey(k.Repos[0]) {
			continue
		}
		for _, ev := range k.Evidence {
			if slices.Contains(loaders, ev.Harness) && ev.Scope.Remote == k.Repos[0] {
				seen[k.Repos[0]] = true
				out = append(out, k.Repos[0])
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// repoNoteHarnessProblem says why an add can't go to a repo note, if it
// can't: only the harnesses in loaders load one, so a lesson none of their
// sessions showed would load nowhere it was seen.
func repoNoteHarnessProblem(p Proposal, repo *Repo, e RunEvidence, loaders []string) string {
	if repo == nil || !repo.RepoNote || p.Kind != "add" {
		return ""
	}
	if len(loaders) == 0 {
		return fmt.Sprintf("it targets the repo note for %s, and no enabled harness loads repo notes", repo.Remote)
	}
	from := "no harness recall knows"
	for _, ref := range e.sessions(p.Candidates) {
		if slices.Contains(loaders, ref.Harness) {
			return ""
		}
		if ref.Harness != "" {
			from = ref.Harness
		}
	}
	return fmt.Sprintf("it targets the repo note for %s, which only %s loads, but no session behind it came from there; they came from %s", repo.Remote, harnessList(loaders), from)
}

// harnessList names harnesses for a sentence: "claude", "claude or kiro".
func harnessList(names []string) string {
	return strings.Join(names, " or ")
}

// RepoNoteContext is what the SessionStart hook adds for a session that
// starts in cwd: the repo note for the repo holding it, under one heading
// naming that repo, or "" when there is none.
func RepoNoteContext(dir, cwd, home string) (string, error) {
	if dir == "" || cwd == "" {
		return "", nil
	}
	_, remote := LocateCheckout(home)(cwd)
	path := RepoNotePath(dir, remote)
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	body := strings.TrimSpace(string(data))
	// The hook's heading names the repo; a note's own top heading would
	// repeat it.
	if first, rest, _ := strings.Cut(body, "\n"); strings.HasPrefix(first, "# ") {
		body = strings.TrimSpace(rest)
	}
	if body == "" {
		return "", nil
	}
	return fmt.Sprintf("# Rules for %s\n\nThe owner keeps these rules for this repo outside it; they apply only here.\n\n%s\n", remote, body), nil
}
