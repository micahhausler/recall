package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// schema lists every allowed leaf key; * matches one name segment.
// A new key goes here, in config.example.toml, and in Settings.
var schema = []string{
	"host",
	"store.kind",
	"store.path",
	"store.s3.profile",
	"store.s3.region",
	"store.s3.bucket",
	"store.s3.bucket_prefix",
	"store.s3.endpoint",
	"store.s3.retention_days",
	"archive-chats.lookback",
	"archive-chats.price_per_gb",
	"harness.*.source",
	"harness.*.enabled",
	"recur.prefix",
	"recur.env",
	"recur.cron_tz",
	"recur.jobs.*.schedule",
	"recur.jobs.*.command",
	"recur.jobs.*.enabled",
	"learn.runner",
	"learn.map_model",
	"learn.reduce_model",
	"learn.targets",
	"learn.ledger",
	"learn.retire_days",
	"learn.map_workers",
	"learn.apply_model",
	"learn.skills",
	"learn.bin",
	"learn.review",
	"learn.check",
	"lint.ignore",
	"keep.targets",
	"keep.bin",
	"keep.public",
	"extensions.*",
}

func matches(pattern, key string) bool {
	ps, ks := strings.Split(pattern, "."), strings.Split(key, ".")
	if len(ps) != len(ks) {
		return false
	}
	for i := range ps {
		if ps[i] != "*" && ps[i] != ks[i] {
			return false
		}
	}
	return true
}

// validate rejects any merged leaf key the schema doesn't allow, naming the
// file and line that set it.
func (c *Config) validate() error {
	for _, key := range c.Leaves() {
		allowed := false
		for _, pattern := range schema {
			if matches(pattern, key) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("%s: unknown key %q", c.origin[key], key)
		}
	}
	return nil
}

// Settings is the typed view of the merged config, defaults applied.
type Settings struct {
	Host      string
	Store     StoreSettings
	Archive   ArchiveSettings
	Harnesses map[string]HarnessSettings
	Recur     RecurSettings
	Learn     LearnSettings
	Lint      LintSettings
	Keep      KeepSettings
	// Extensions are the owner's instruction files per extension point (a
	// skill name, or learn-reduce), home-expanded, in order.
	Extensions map[string][]string
}

// LintSettings configure the staleness lint.
type LintSettings struct {
	// Ignore lists paths lint never reports, such as a side file that
	// sparse checkout deliberately keeps off this machine.
	Ignore []string
}

// KeepSettings are the keep skill's keys the binary reads too.
type KeepSettings struct {
	// Public lists paths shared publicly, where nothing personal goes; the
	// apply step marks a worktree under one as public.
	Public []string
}

// LearnSettings configure the learning loop (docs/learning.md).
type LearnSettings struct {
	// Runner is the harness CLI model calls go through.
	Runner string
	// MapModel reads one session; ReduceModel folds a window into proposals.
	MapModel    string
	ReduceModel string
	// Targets are the context files rules live in.
	Targets []string
	// Ledger is the sidecar file tracking each rule.
	Ledger string
	// RetireDays: a rule with no relevance for this long is proposed for
	// retirement.
	RetireDays int
	// MapWorkers is how many sessions the map step observes at once,
	// MinMapWorkers to MaxMapWorkers.
	MapWorkers int
	// ApplyModel edits worktrees with the reviewed proposals.
	ApplyModel string
	// Skills is where a new skill lands, as <Skills>/<name>/SKILL.md; Bin is
	// where a graduated script lands.
	Skills string
	Bin    string
	// Review is the publisher an applied change goes to: pr or branch.
	Review string
	// Check is a shell command run in each changed worktree with the staged
	// diff on stdin; a nonzero exit holds that repo's change back.
	Check string
}

type StoreSettings struct {
	Kind string
	Path string // fs only
	S3   S3Settings
}

type S3Settings struct {
	Profile       string
	Region        string
	Bucket        string
	BucketPrefix  string
	Endpoint      string
	RetentionDays int
}

type ArchiveSettings struct {
	Lookback   int
	PricePerGB float64
}

type HarnessSettings struct {
	Source  string
	Enabled bool
}

type RecurSettings struct {
	Prefix string
	Env    []string
	CronTZ string
	Jobs   map[string]Job
}

type Job struct {
	Name     string
	Schedule string
	Command  string
	Source   Origin // where the command was set
}

// Settings decodes the merged config into the typed view. home expands ~ in
// paths and hostname fills the host default.
func (c *Config) Settings(home, hostname string) (*Settings, error) {
	s := &Settings{
		Host: hostname,
		Store: StoreSettings{
			Kind: "fs",
			Path: filepath.Join(home, "recall-store"),
			S3:   S3Settings{BucketPrefix: "chat-archive", RetentionDays: 3650},
		},
		Archive:   ArchiveSettings{Lookback: 4, PricePerGB: 0.023},
		Harnesses: map[string]HarnessSettings{},
		Recur:     RecurSettings{Prefix: "recall.recur", Jobs: map[string]Job{}},
		Learn: LearnSettings{
			Runner:      "claude",
			MapModel:    "sonnet",
			ReduceModel: "opus",
			Targets:     []string{filepath.Join(home, ".claude", "CLAUDE.md")},
			Ledger:      filepath.Join(home, ".claude", "recall-ledger.json"),
			RetireDays:  90,
			MapWorkers:  4,
			ApplyModel:  "sonnet",
			Skills:      filepath.Join(home, ".claude", "skills"),
			Bin:         filepath.Join(home, "bin"),
			Review:      "pr",
		},
	}
	var err error
	str := func(key string, dst *string) {
		if v, ok := c.Get(key); ok && err == nil {
			sv, ok := v.(string)
			if !ok {
				err = c.typeErr(key, "a string", v)
				return
			}
			*dst = sv
		}
	}
	num := func(key string, dst *int) {
		if v, ok := c.Get(key); ok && err == nil {
			iv, ok := v.(int64)
			if !ok {
				err = c.typeErr(key, "an integer", v)
				return
			}
			*dst = int(iv)
		}
	}
	flt := func(key string, dst *float64) {
		if v, ok := c.Get(key); ok && err == nil {
			switch fv := v.(type) {
			case float64:
				*dst = fv
			case int64:
				*dst = float64(fv)
			default:
				err = c.typeErr(key, "a number", v)
			}
		}
	}
	path := func(key string, dst *string) {
		str(key, dst)
		*dst = ExpandHome(*dst, home)
	}
	str("host", &s.Host)
	str("store.kind", &s.Store.Kind)
	path("store.path", &s.Store.Path)
	str("store.s3.profile", &s.Store.S3.Profile)
	str("store.s3.region", &s.Store.S3.Region)
	str("store.s3.bucket", &s.Store.S3.Bucket)
	str("store.s3.bucket_prefix", &s.Store.S3.BucketPrefix)
	str("store.s3.endpoint", &s.Store.S3.Endpoint)
	num("store.s3.retention_days", &s.Store.S3.RetentionDays)
	num("archive-chats.lookback", &s.Archive.Lookback)
	flt("archive-chats.price_per_gb", &s.Archive.PricePerGB)
	str("recur.prefix", &s.Recur.Prefix)
	str("recur.cron_tz", &s.Recur.CronTZ)
	if v, ok := c.Get("lint.ignore"); ok && err == nil {
		list, lok := v.([]any)
		if !lok {
			err = c.typeErr("lint.ignore", "an array of strings", v)
		} else {
			for _, e := range list {
				ev, sok := e.(string)
				if !sok {
					err = c.typeErr("lint.ignore", "an array of strings", e)
					break
				}
				s.Lint.Ignore = append(s.Lint.Ignore, ExpandHome(ev, home))
			}
		}
	}
	str("learn.runner", &s.Learn.Runner)
	str("learn.map_model", &s.Learn.MapModel)
	str("learn.reduce_model", &s.Learn.ReduceModel)
	path("learn.ledger", &s.Learn.Ledger)
	num("learn.retire_days", &s.Learn.RetireDays)
	num("learn.map_workers", &s.Learn.MapWorkers)
	str("learn.apply_model", &s.Learn.ApplyModel)
	path("learn.skills", &s.Learn.Skills)
	path("learn.bin", &s.Learn.Bin)
	str("learn.review", &s.Learn.Review)
	str("learn.check", &s.Learn.Check)
	if v, ok := c.Get("keep.public"); ok && err == nil {
		list, lok := v.([]any)
		if !lok {
			err = c.typeErr("keep.public", "an array of strings", v)
		} else {
			for _, e := range list {
				ev, sok := e.(string)
				if !sok {
					err = c.typeErr("keep.public", "an array of strings", e)
					break
				}
				s.Keep.Public = append(s.Keep.Public, ExpandHome(ev, home))
			}
		}
	}
	if err == nil && (s.Learn.MapWorkers < MinMapWorkers || s.Learn.MapWorkers > MaxMapWorkers) {
		o, _ := c.Origin("learn.map_workers")
		err = fmt.Errorf("%s: learn.map_workers must be %d to %d, got %d", o, MinMapWorkers, MaxMapWorkers, s.Learn.MapWorkers)
	}
	if v, ok := c.Get("learn.targets"); ok && err == nil {
		list, lok := v.([]any)
		if !lok {
			err = c.typeErr("learn.targets", "an array of strings", v)
		} else {
			s.Learn.Targets = nil
			for _, e := range list {
				ev, sok := e.(string)
				if !sok {
					err = c.typeErr("learn.targets", "an array of strings", e)
					break
				}
				s.Learn.Targets = append(s.Learn.Targets, ExpandHome(ev, home))
			}
		}
	}
	if v, ok := c.Get("recur.env"); ok && err == nil {
		list, lok := v.([]any)
		if !lok {
			err = c.typeErr("recur.env", "an array of strings", v)
		}
		for _, e := range list {
			ev, sok := e.(string)
			if !sok {
				err = c.typeErr("recur.env", "an array of strings", e)
				break
			}
			s.Recur.Env = append(s.Recur.Env, ev)
		}
	}
	for _, name := range c.tableNames("extensions") {
		if err != nil {
			break
		}
		key := "extensions." + name
		v, _ := c.Get(key)
		list, lok := v.([]any)
		if !lok {
			err = c.typeErr(key, "an array of file paths", v)
			break
		}
		if s.Extensions == nil {
			s.Extensions = map[string][]string{}
		}
		for _, e := range list {
			ev, sok := e.(string)
			if !sok {
				err = c.typeErr(key, "an array of file paths", e)
				break
			}
			s.Extensions[name] = append(s.Extensions[name], ExpandHome(ev, home))
		}
	}
	for _, name := range c.tableNames("harness") {
		h := HarnessSettings{Enabled: true}
		if name == "claude" {
			h.Source = filepath.Join(home, ".claude", "projects")
		}
		base := "harness." + name
		path(base+".source", &h.Source)
		c.boolKey(base+".enabled", &h.Enabled, &err)
		if h.Enabled {
			s.Harnesses[name] = h
		}
	}
	if _, declared := c.Get("harness.claude"); !declared {
		s.Harnesses["claude"] = HarnessSettings{Source: filepath.Join(home, ".claude", "projects"), Enabled: true}
	}
	for _, name := range c.tableNames("recur.jobs") {
		j := Job{Name: name}
		enabled := true
		base := "recur.jobs." + name
		str(base+".schedule", &j.Schedule)
		str(base+".command", &j.Command)
		c.boolKey(base+".enabled", &enabled, &err)
		if !enabled {
			continue
		}
		j.Source, _ = c.Origin(base + ".command")
		if err == nil && j.Schedule == "" {
			o, _ := c.Origin(base + ".command")
			err = fmt.Errorf("%s: job %q has no schedule", o, name)
		}
		if err == nil && j.Command == "" {
			o, _ := c.Origin(base + ".schedule")
			err = fmt.Errorf("%s: job %q has no command", o, name)
		}
		s.Recur.Jobs[name] = j
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// The bounds on learn.map_workers: at least one, and few enough that the
// harness's rate limits aren't the thing being tested.
const (
	MinMapWorkers = 1
	MaxMapWorkers = 32
)

func (c *Config) boolKey(key string, dst *bool, err *error) {
	if v, ok := c.Get(key); ok && *err == nil {
		bv, bok := v.(bool)
		if !bok {
			*err = c.typeErr(key, "a bool", v)
			return
		}
		*dst = bv
	}
}

func (c *Config) typeErr(key, want string, got any) error {
	o, _ := c.Origin(key)
	return fmt.Errorf("%s: %s wants %s, got %T", o, key, want, got)
}

func (c *Config) tableNames(prefix string) []string {
	v, ok := c.Get(prefix)
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// ExpandHome expands a leading ~ or $HOME in a path.
func ExpandHome(p, home string) string {
	switch {
	case p == "~" || p == "$HOME":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "$HOME/"):
		return filepath.Join(home, p[len("$HOME/"):])
	}
	return os.Expand(p, func(name string) string {
		if name == "HOME" {
			return home
		}
		return os.Getenv(name)
	})
}
