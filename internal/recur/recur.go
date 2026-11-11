package recur

import (
	"fmt"
	"io"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/micahhausler/recall/internal/config"
)

// Setup is everything a Scheduler needs besides the jobs themselves.
type Setup struct {
	Prefix string
	CronTZ string
	// Env is NAME=value for every job run.
	Env map[string]string
	// Self is the recall binary the scheduler invokes.
	Self string
	// ConfigEnv is $RECALL_CONFIG when set: written into what the scheduler
	// runs, so scheduled runs read the same config.
	ConfigEnv string
	// Home is the user's home directory; StateDir and AgentsDir hang off it.
	Home string
}

// Action is one planned change; Op is add, update, load, remove, or rewrite.
type Action struct {
	Op   string
	What string
}

// Scheduler reconciles declared jobs with one platform's actual state.
// Backends register under the name config uses ("cron", "launchd").
type Scheduler interface {
	// Sync computes the difference and applies it; with check, it changes
	// nothing and also lists each job as installed. Plan lines and ok lines
	// go to out.
	Sync(jobs map[string]config.Job, check bool, out io.Writer) ([]Action, error)
}

var registry = map[string]func(Setup) Scheduler{}

// Register adds a backend under its name.
func Register(name string, f func(Setup) Scheduler) {
	registry[name] = f
}

// ForOS names the backend for an operating system.
func ForOS() string {
	if runtime.GOOS == "darwin" {
		return "launchd"
	}
	return "cron"
}

// New opens a registered backend.
func New(name string, s Setup) (Scheduler, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown scheduler %q", name)
	}
	return f(s), nil
}

var envRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// LoadSetup validates the recur settings and jobs.
func LoadSetup(s *config.Settings, self, configEnv, home string) (Setup, error) {
	set := Setup{Prefix: s.Recur.Prefix, CronTZ: s.Recur.CronTZ, Env: map[string]string{},
		Self: self, ConfigEnv: configEnv, Home: home}
	if !NameRE.MatchString(set.Prefix) {
		return set, fmt.Errorf("bad recur.prefix %q", set.Prefix)
	}
	for _, e := range s.Recur.Env {
		m := envRE.FindStringSubmatch(e)
		if m == nil {
			return set, fmt.Errorf("recur.env wants NAME=value, got %q", e)
		}
		set.Env[m[1]] = m[2]
	}
	for name, j := range s.Recur.Jobs {
		if !NameRE.MatchString(name) {
			return set, fmt.Errorf("%s: bad job name %q", j.Source, name)
		}
		if _, err := ValidateSchedule(j.Schedule); err != nil {
			return set, fmt.Errorf("%s: job %q: %v", j.Source, name, err)
		}
	}
	return set, nil
}

// runArgs is the argv the scheduler runs for a job: the current config is
// re-read then, so command and env changes take effect without a sync.
func (s Setup) runArgs(name string) []string {
	return []string{s.Self, "recur", "run", name}
}

func sortedJobs(jobs map[string]config.Job) []config.Job {
	names := make([]string, 0, len(jobs))
	for n := range jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]config.Job, 0, len(names))
	for _, n := range names {
		out = append(out, jobs[n])
	}
	return out
}

var bareRE = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shQuote quotes one argument for /bin/sh, like Python's shlex.quote.
func shQuote(s string) string {
	if s != "" && bareRE.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
