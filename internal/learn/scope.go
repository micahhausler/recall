package learn

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/micahhausler/recall/internal/events"
	"github.com/micahhausler/recall/internal/review"
)

// Scope is where a session ran: its working directory, the git checkout
// that holds it, and that checkout's origin. A lesson from one repo belongs
// in that repo's context file, not a global one, and only a session's scope
// can tell the two apart.
type Scope struct {
	// CWD is the directory most of the session's turns ran in, and Repo the
	// checkout that holds it; both are written relative to the home
	// directory (~/...) so no user name reaches the store. A session in a
	// git worktree belongs to the worktree's main checkout.
	CWD  string `json:"cwd,omitempty"`
	Repo string `json:"repo,omitempty"`
	// Remote is the checkout's origin as host/path, like
	// example.com/you/project: the same repo on any machine.
	Remote string `json:"remote,omitempty"`
}

// Key names the repo a session ran in: its origin when it has one, since
// that is the same on every machine, and its path otherwise. Empty means
// the session ran outside any checkout the map could see.
func (s Scope) Key() string {
	if s.Remote != "" {
		return s.Remote
	}
	return s.Repo
}

// Locator finds the git checkout that holds a directory on this machine,
// and its origin. Empty root means none does.
type Locator func(dir string) (root, remote string)

// scopeOf works out where a session ran. home is this machine's home
// directory, and locate looks a directory up on this machine; both are
// unset for a session read from another host's archive, whose paths mean
// nothing here.
// An event's repo, when the source recorded one, is the session's remote
// whatever the lookup finds.
func scopeOf(evs []events.Event, home string, locate Locator) Scope {
	repo := dominant(evs, func(e events.Event) string { return e.Repo })
	cwd := dominantCWD(evs)
	if cwd == "" {
		return Scope{Remote: repo}
	}
	if locate == nil {
		return Scope{CWD: foreignHome(cwd), Remote: repo}
	}
	sc := Scope{CWD: homeRelative(cwd, home)}
	if root, remote := locate(cwd); root != "" {
		sc.Repo, sc.Remote = homeRelative(root, home), remote
	}
	if repo != "" {
		sc.Remote = repo
	}
	return sc
}

// dominantCWD is the directory most of a session's own turns ran in; the
// earliest wins a tie.
func dominantCWD(evs []events.Event) string {
	return dominant(evs, func(e events.Event) string { return e.CWD })
}

// dominant is the value of field most of a session's own turns carry; the
// earliest wins a tie.
func dominant(evs []events.Event, field func(events.Event) string) string {
	count := map[string]int{}
	best := ""
	for _, e := range evs {
		v := field(e)
		if e.Sidechain || v == "" {
			continue
		}
		count[v]++
		if best == "" || count[v] > count[best] {
			best = v
		}
	}
	return best
}

// homeRelative writes a path under home as ~/..., and leaves others alone.
func homeRelative(path, home string) string {
	if home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~/" + filepath.ToSlash(rest)
	}
	return path
}

var foreignHomeRE = regexp.MustCompile(`^/(?:home|Users)/[^/]+(/.*)?$`)

// foreignHome writes another machine's path under a home directory as
// ~/..., by the two usual layouts, so its user name stays out of the store.
func foreignHome(path string) string {
	if m := foreignHomeRE.FindStringSubmatch(path); m != nil {
		return "~" + m[1]
	}
	return path
}

// ExpandScopePath turns a scope's ~/ path back into a path on this machine.
func ExpandScopePath(path, home string) string {
	if path == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(home, filepath.FromSlash(rest))
	}
	return path
}

// LocateCheckout returns the Locator for this machine's filesystem. It
// reads .git directly and runs nothing. A directory that no longer exists,
// such as a removed worktree, is looked up from its nearest existing
// ancestor. home, when set, bounds the walk: a checkout whose root is the
// home directory claims no session, so a dotfiles repo at ~/.git doesn't
// claim everything under it.
func LocateCheckout(home string) Locator {
	if home != "" {
		home = filepath.Clean(home)
	}
	return func(dir string) (root, remote string) {
		return locateCheckout(dir, home)
	}
}

func locateCheckout(dir, home string) (root, remote string) {
	dir = filepath.Clean(dir)
	for {
		if home != "" && dir == home {
			return "", ""
		}
		st, err := os.Lstat(filepath.Join(dir, ".git"))
		switch {
		case err == nil && st.IsDir():
			return dir, originOf(filepath.Join(dir, ".git", "config"))
		case err == nil && st.Mode().IsRegular():
			// A worktree or submodule: .git is a file naming the real git
			// directory.
			gitdir := readGitdir(filepath.Join(dir, ".git"), dir)
			if gitdir == "" {
				return "", ""
			}
			// A linked worktree's git directory names the main checkout's
			// in its commondir file; a submodule's has none.
			if common := readCommonDir(gitdir); common != "" {
				if filepath.Base(common) == ".git" {
					return filepath.Dir(common), originOf(filepath.Join(common, "config"))
				}
				// The main repository is bare: the worktree is the only
				// checkout, but the config still lives in the common dir.
				return dir, originOf(filepath.Join(common, "config"))
			}
			return dir, originOf(filepath.Join(gitdir, "config"))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

// readCommonDir reads a worktree git directory's commondir file: the main
// checkout's git directory, relative to the worktree's own when not
// absolute. Empty means there is none, as in a submodule.
func readCommonDir(gitdir string) string {
	data, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(data))
	if common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	return filepath.Clean(common)
}

func readGitdir(file, dir string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	rest, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir := strings.TrimSpace(rest)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	return filepath.Clean(gitdir)
}

// originOf reads the origin remote's URL from a git config file and returns
// it as host/path.
func originOf(config string) string {
	f, err := os.Open(config)
	if err != nil {
		return ""
	}
	defer f.Close()
	inOrigin := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = strings.Join(strings.Fields(line), " ") == `[remote "origin"]`
			continue
		}
		if !inOrigin {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok && strings.TrimSpace(key) == "url" {
			return review.RemoteSlug(strings.TrimSpace(value))
		}
	}
	return ""
}

// harnessScope says why an add can't go to its target, if it can't, when
// every session behind it came from one harness: the target is read by
// another enabled harness too, or the harness has no context file of its
// own. Repo scope comes first, so a repo's file is never checked; outside
// evidence names no harness; and only an add narrows. readers maps each
// enabled harness to the files it loads, and own a harness to the file its
// lessons go in.
func harnessScope(p Proposal, target string, repos []Repo, e RunEvidence, readers map[string][]string, own map[string]string, home string) string {
	if p.Kind != "add" || len(readers) < 2 || target == "" || repoOf(repos, target) != nil {
		return ""
	}
	from := ""
	for _, ref := range e.sessions(p.Candidates) {
		switch {
		case strings.HasPrefix(ref.Harness, "evidence-"):
			continue
		case ref.Harness == "":
			return "" // recorded before refs named their harness
		case from != "" && ref.Harness != from:
			return ""
		}
		from = ref.Harness
	}
	if from == "" {
		return ""
	}
	if _, enabled := readers[from]; !enabled {
		return ""
	}
	if len(readers[from]) == 0 {
		return fmt.Sprintf("every session behind it came from %s, which loads no context file of its own; a lesson only %s's sessions show has no file to land in", from, from)
	}
	reads := func(name, path string) bool {
		path = normReader(path, home)
		return slices.ContainsFunc(readers[name], func(f string) bool { return normReader(f, home) == path })
	}
	var others []string
	for name := range readers {
		if name != from && reads(name, target) {
			others = append(others, name)
		}
	}
	if len(others) == 0 {
		return ""
	}
	sort.Strings(others)
	where := "a file only " + from + " loads"
	if t := own[from]; t != "" {
		shared := false
		for name := range readers {
			shared = shared || (name != from && reads(name, t))
		}
		if !shared {
			where = homeRelative(t, home)
		}
	}
	return fmt.Sprintf("every session behind it came from %s, and %s loads %s too; it belongs in %s", from,
		strings.Join(others, " and "), homeRelative(normReader(target, home), home), where)
}

// normReader writes a path the way targets and readers are compared.
func normReader(p, home string) string {
	return review.Resolve(normTarget(p, home))
}
