package learn

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
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
func scopeOf(evs []events.Event, home string, locate Locator) Scope {
	cwd := dominantCWD(evs)
	if cwd == "" {
		return Scope{}
	}
	if locate == nil {
		return Scope{CWD: foreignHome(cwd)}
	}
	sc := Scope{CWD: homeRelative(cwd, home)}
	if root, remote := locate(cwd); root != "" {
		sc.Repo, sc.Remote = homeRelative(root, home), remote
	}
	return sc
}

// dominantCWD is the directory most of a session's own turns ran in; the
// earliest wins a tie.
func dominantCWD(evs []events.Event) string {
	count := map[string]int{}
	best := ""
	for _, e := range evs {
		if e.Sidechain || e.CWD == "" {
			continue
		}
		count[e.CWD]++
		if best == "" || count[e.CWD] > count[best] {
			best = e.CWD
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
