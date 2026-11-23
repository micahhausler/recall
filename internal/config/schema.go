package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	"archive-chats.redact",
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
	"learn.repos",
	"learn.ledger",
	"learn.candidates",
	"learn.retire_days",
	"learn.window_days",
	"learn.budget",
	"learn.map_workers",
	"learn.apply_model",
	"learn.skills",
	"learn.bin",
	"learn.settings",
	"learn.review",
	"learn.check",
	"learn.require_owner_evidence",
	"learn.repeat_evidence_sessions",
	"learn.replay",
	"learn.evidence",
	"learn.evidence_trailer",
	"hooks.map",
	"hooks.archive",
	"hooks.learn_every",
	"lint.ignore",
	"keep.targets",
	"keep.bin",
	"keep.public",
	"review.assignees",
	"review.credentials.command",
	"review.credentials.timeout",
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

// removedEncryptKey matches the leaf keys of the removed client-side
// encryption feature by segment, so the error below is recall's only word
// on them.
func removedEncryptKey(key string) bool {
	ks := strings.Split(key, ".")
	return len(ks) >= 2 && ks[0] == "store" && ks[1] == "encrypt"
}

// validate rejects any merged leaf key the schema doesn't allow, naming the
// file and line that set it.
func (c *Config) validate() error {
	for _, key := range c.Leaves() {
		if removedEncryptKey(key) {
			return fmt.Errorf("%s: %q sets client-side encryption of the archive, a feature recall removed; the bucket's own encryption and access policy protect the store, so delete the key", c.origin[key], key)
		}
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
	Hooks     HookSettings
	Review    ReviewSettings
	// Extensions are the owner's instruction files per extension point (a
	// skill name, or learn-reduce), home-expanded, in order.
	Extensions map[string][]string
}

// HookSettings say what the plugin's session hooks may do in the
// background when a session ends. All are off by default: the hooks are
// installed with the plugin, and each step spends money or bandwidth only
// when the owner turns it on.
type HookSettings struct {
	// Map observes sessions that have gone quiet, with the map model.
	Map bool
	// Archive uploads missing weeks to the store, at most once a day.
	Archive bool
	// LearnEvery is how many days apart the reduce and apply run; 0 never.
	LearnEvery int
}

// ReviewSettings say how recall reaches the repositories it sends changes
// to for review.
type ReviewSettings struct {
	// Assignees are the logins assigned to each pull request the pr
	// publisher opens. Empty assigns no one.
	Assignees   []string
	Credentials CredentialSettings
}

// CredentialSettings name a command that supplies a credential per
// repository (docs/pr-credentials.md), for the pr publisher and the github
// evidence source to act as in place of whatever gh is signed in as. With
// no command, both use the machine's own gh login and git identity.
type CredentialSettings struct {
	// Command is the program and its arguments, run without a shell.
	Command []string
	// Timeout bounds one run of the command.
	Timeout time.Duration
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
	// Repos are the checkouts whose own context files a repo-only lesson
	// may land in.
	Repos []string
	// Ledger is the separate file, kept beside the context files, that
	// tracks each rule.
	Ledger string
	// Candidates is the candidate ledger: the lessons sessions have shown
	// that aren't rules yet, and which of them a proposal already used.
	Candidates string
	// RetireDays: a rule with no relevance for this long is proposed for
	// retirement.
	RetireDays int
	// WindowDays is how far back the reduce reads observations; 0, the
	// default, reads RetireDays back, today's behavior. Retirement always
	// uses RetireDays, so a wider window never retires rules faster.
	WindowDays int
	// Budget is how many tokens one target file may cost, estimated at
	// four characters a token; 0 is no budget.
	Budget int
	// MapWorkers is how many sessions the map step observes at once,
	// MinMapWorkers to MaxMapWorkers.
	MapWorkers int
	// ApplyModel edits worktrees with the reviewed proposals.
	ApplyModel string
	// Skills is where a new skill lands, as <Skills>/<name>/SKILL.md; Bin is
	// where a graduated script lands.
	Skills string
	Bin    string
	// Settings is the harness settings file a graduated hook or permission
	// rule goes in; empty turns those two off.
	Settings string
	// Review is the publisher an applied change goes to: pr or branch.
	Review string
	// Check is a shell command run in each changed worktree with the staged
	// diff on stdin; a nonzero exit holds that repo's change back.
	Check string
	// RequireOwnerEvidence holds back a new rule or skill that cites no
	// observation quoting words the owner typed.
	RequireOwnerEvidence bool
	// RepeatEvidenceSessions, when above 0, lets a new rule rest on repeat
	// evidence alone when the binary counted the repeat in at least that
	// many sessions; 0 turns that off.
	RepeatEvidenceSessions int
	// Replay asks, in one tool-less model call that executes nothing, what
	// the agent would do next at the moment that earned a proposed rule,
	// with and without the rule, and holds back an add the replay shows
	// changing nothing. It also replays, without them, rules
	// earned only on models no recent session ran, and proposes retiring
	// one the current model follows unprompted in every replay. Each replay
	// is a model call, so it is off by default.
	Replay bool
	// Evidence names the sources outside the chat the map reads, such as
	// "github": review comments and failed checks on agent-authored pull
	// requests in the learn.repos origins. Empty, the default, reads none.
	Evidence []string
	// EvidenceTrailer is the commit trailer that marks an agent-authored
	// pull request to an evidence source. Set it when your harness writes
	// a different trailer, or attribution is off and you add your own.
	EvidenceTrailer string
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
	// Redact replaces secret-shaped tokens in a week's text files as its
	// tarball is built.
	Redact bool
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
		// The credential package's DefaultTimeout.
		Review: ReviewSettings{Credentials: CredentialSettings{Timeout: 30 * time.Second}},
		Learn: LearnSettings{
			Runner:      "claude",
			MapModel:    "sonnet",
			ReduceModel: "opus",
			Targets:     []string{filepath.Join(home, ".claude", "CLAUDE.md")},
			Ledger:      DefaultLedger("claude", home),
			Candidates:  filepath.Join(home, ".local", "state", "recall", "learn", "candidates.json"),
			RetireDays:  90,
			MapWorkers:  4,
			ApplyModel:  "sonnet",
			Skills:      DefaultSkills("claude", home),
			Bin:         filepath.Join(home, "bin"),
			Review:      "pr",

			RequireOwnerEvidence: true,
			// The default agent trailer (evidence.Trailer).
			EvidenceTrailer: DefaultTrailer("claude"),
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
	c.boolKey("archive-chats.redact", &s.Archive.Redact, &err)
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
	c.boolKey("hooks.map", &s.Hooks.Map, &err)
	c.boolKey("hooks.archive", &s.Hooks.Archive, &err)
	num("hooks.learn_every", &s.Hooks.LearnEvery)
	str("learn.runner", &s.Learn.Runner)
	str("learn.map_model", &s.Learn.MapModel)
	str("learn.reduce_model", &s.Learn.ReduceModel)
	path("learn.ledger", &s.Learn.Ledger)
	path("learn.candidates", &s.Learn.Candidates)
	num("learn.retire_days", &s.Learn.RetireDays)
	num("learn.window_days", &s.Learn.WindowDays)
	num("learn.budget", &s.Learn.Budget)
	num("learn.map_workers", &s.Learn.MapWorkers)
	str("learn.apply_model", &s.Learn.ApplyModel)
	path("learn.skills", &s.Learn.Skills)
	path("learn.bin", &s.Learn.Bin)
	path("learn.settings", &s.Learn.Settings)
	str("learn.review", &s.Learn.Review)
	if err == nil && s.Learn.Review == "inbox" {
		// This review channel was removed; branch reviews locally too, and
		// needs no remote and no gh.
		o, _ := c.Origin("learn.review")
		err = fmt.Errorf("%s: learn.review %q was removed; use \"branch\" for a local branch with no remote, or \"pr\"", o, s.Learn.Review)
	}
	str("learn.check", &s.Learn.Check)
	str("learn.evidence_trailer", &s.Learn.EvidenceTrailer)
	c.boolKey("learn.require_owner_evidence", &s.Learn.RequireOwnerEvidence, &err)
	num("learn.repeat_evidence_sessions", &s.Learn.RepeatEvidenceSessions)
	c.boolKey("learn.replay", &s.Learn.Replay, &err)
	if v, ok := c.Get("learn.evidence"); ok && err == nil {
		list, lok := v.([]any)
		if !lok {
			err = c.typeErr("learn.evidence", "an array of strings", v)
		} else {
			for _, e := range list {
				ev, sok := e.(string)
				if !sok {
					err = c.typeErr("learn.evidence", "an array of strings", e)
					break
				}
				s.Learn.Evidence = append(s.Learn.Evidence, ev)
			}
		}
	}
	if v, ok := c.Get("review.assignees"); ok && err == nil {
		list, lok := v.([]any)
		if !lok {
			err = c.typeErr("review.assignees", "an array of strings: the logins to assign", v)
		} else {
			for _, e := range list {
				ev, sok := e.(string)
				if !sok {
					err = c.typeErr("review.assignees", "an array of strings", e)
					break
				}
				if ev == "" || strings.ContainsAny(ev, " \t@") {
					o, _ := c.Origin("review.assignees")
					err = fmt.Errorf("%s: review.assignees %q isn't a login; write it bare, like \"you\"", o, ev)
					break
				}
				s.Review.Assignees = append(s.Review.Assignees, ev)
			}
		}
	}
	if v, ok := c.Get("review.credentials.command"); ok && err == nil {
		list, lok := v.([]any)
		if !lok {
			err = c.typeErr("review.credentials.command", "an array of strings: the program, then its arguments", v)
		} else {
			for i, e := range list {
				ev, sok := e.(string)
				if !sok {
					err = c.typeErr("review.credentials.command", "an array of strings", e)
					break
				}
				if i == 0 {
					ev = ExpandHome(ev, home)
				}
				s.Review.Credentials.Command = append(s.Review.Credentials.Command, ev)
			}
			if err == nil && (len(s.Review.Credentials.Command) == 0 || s.Review.Credentials.Command[0] == "") {
				o, _ := c.Origin("review.credentials.command")
				err = fmt.Errorf("%s: review.credentials.command names no program; delete the key to use the machine's own gh login", o)
			}
		}
	}
	if v, ok := c.Get("review.credentials.timeout"); ok && err == nil {
		o, _ := c.Origin("review.credentials.timeout")
		tv, sok := v.(string)
		if !sok {
			err = c.typeErr("review.credentials.timeout", "a duration string like \"30s\"", v)
		} else if d, perr := time.ParseDuration(tv); perr != nil || d <= 0 {
			err = fmt.Errorf("%s: review.credentials.timeout %q isn't a positive duration like \"30s\"", o, tv)
		} else {
			s.Review.Credentials.Timeout = d
		}
	}
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
	if v, ok := c.Get("learn.repos"); ok && err == nil {
		list, lok := v.([]any)
		if !lok {
			err = c.typeErr("learn.repos", "an array of strings", v)
		} else {
			for _, e := range list {
				ev, sok := e.(string)
				if !sok {
					err = c.typeErr("learn.repos", "an array of strings", e)
					break
				}
				s.Learn.Repos = append(s.Learn.Repos, ExpandHome(ev, home))
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
		h := HarnessSettings{Enabled: true, Source: DefaultSource(name, home)}
		base := "harness." + name
		path(base+".source", &h.Source)
		c.boolKey(base+".enabled", &h.Enabled, &err)
		if h.Enabled {
			s.Harnesses[name] = h
		}
	}
	if _, declared := c.Get("harness.claude"); !declared {
		s.Harnesses["claude"] = HarnessSettings{Source: DefaultSource("claude", home), Enabled: true}
	}
	if _, set := c.Get("learn.targets"); !set {
		// Each enabled harness's own context file.
		s.Learn.Targets = nil
		for _, name := range KnownHarnesses {
			if _, ok := s.Harnesses[name]; ok {
				s.Learn.Targets = append(s.Learn.Targets, DefaultTarget(name, home))
			}
		}
	}
	// With Claude Code off, the loop runs through the harness that is on and
	// keeps its files in that harness's home, so nothing needs ~/.claude.
	if primary := PrimaryHarness(s.Harnesses); primary != "claude" {
		if _, set := c.Get("learn.runner"); !set {
			s.Learn.Runner = primary
		}
		if _, set := c.Get("learn.ledger"); !set {
			s.Learn.Ledger = DefaultLedger(primary, home)
		}
		if _, set := c.Get("learn.skills"); !set {
			s.Learn.Skills = DefaultSkills(primary, home)
		}
		if _, set := c.Get("learn.evidence_trailer"); !set {
			s.Learn.EvidenceTrailer = DefaultTrailer(primary)
		}
	}
	if s.Learn.Runner == "kiro" {
		// Kiro names its models differently; auto lets it choose.
		if _, set := c.Get("learn.map_model"); !set {
			s.Learn.MapModel = "auto"
		}
		if _, set := c.Get("learn.reduce_model"); !set {
			s.Learn.ReduceModel = "auto"
		}
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

// KnownHarnesses are the harnesses recall has an adapter for, in the order
// PrimaryHarness prefers them.
var KnownHarnesses = []string{"claude", "kiro"}

// PrimaryHarness is the enabled harness whose CLI and home the loop's
// defaults follow: claude when it is on, else the first other known one on,
// else claude.
func PrimaryHarness(enabled map[string]HarnessSettings) string {
	for _, name := range KnownHarnesses {
		if _, ok := enabled[name]; ok {
			return name
		}
	}
	return "claude"
}

// DefaultLedger is the rule ledger's default path for a primary harness,
// in that harness's home.
func DefaultLedger(name, home string) string {
	if name == "kiro" {
		return filepath.Join(home, ".kiro", "recall-ledger.json")
	}
	return filepath.Join(home, ".claude", "recall-ledger.json")
}

// DefaultSkills is where a primary harness loads the owner's own skills
// from, and so where a graduated skill lands.
func DefaultSkills(name, home string) string {
	if name == "kiro" {
		return filepath.Join(home, ".kiro", "skills")
	}
	return filepath.Join(home, ".claude", "skills")
}

// DefaultTrailer is the commit trailer that marks a primary harness's
// commits as agent-authored. Kiro writes none of its own, so its owner has
// it add this one.
func DefaultTrailer(name string) string {
	if name == "kiro" {
		return "Co-Authored-By: Kiro"
	}
	return "Co-Authored-By: Claude"
}

// DefaultSource is where a known harness keeps transcripts; "" for one
// recall doesn't know, which must set harness.<name>.source.
func DefaultSource(name, home string) string {
	switch name {
	case "claude":
		return filepath.Join(home, ".claude", "projects")
	case "kiro":
		return filepath.Join(home, ".kiro", "sessions")
	}
	return ""
}

// DefaultTarget is the context file a known harness loads in every session
// and the owner edits: CLAUDE.md for Claude Code, the global AGENTS.md
// steering file for Kiro.
func DefaultTarget(name, home string) string {
	switch name {
	case "claude":
		return filepath.Join(home, ".claude", "CLAUDE.md")
	case "kiro":
		return filepath.Join(home, ".kiro", "steering", "AGENTS.md")
	}
	return ""
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
