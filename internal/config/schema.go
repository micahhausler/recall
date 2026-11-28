package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

// schema lists every allowed leaf key; * matches one name segment.
// A new key goes here, in config.reference.toml, and in Settings.
var schema = []string{
	"host",
	"store.kind",
	"store.path",
	"store.s3.profile",
	"store.s3.region",
	"store.s3.bucket",
	"store.s3.endpoint",
	"store.s3.retention_days",
	"archive-chats.redact",
	"harness.*.source",
	"harness.*.enabled",
	"harness.*.format",
	"harness.*.import.command",
	"harness.*.import.timeout",
	"recur.env",
	"recur.cron_tz",
	"recur.jobs.*.schedule",
	"recur.jobs.*.command",
	"recur.jobs.*.enabled",
	"learn.runner",
	"learn.map_model",
	"learn.map_effort",
	"learn.reduce_model",
	"learn.reduce_effort",
	"learn.targets",
	"learn.repos",
	"learn.repo_notes",
	"learn.retire_days",
	"learn.budget",
	"learn.map_workers",
	"learn.apply_model",
	"learn.apply_effort",
	"learn.skills",
	"learn.bin",
	"learn.settings",
	"learn.review",
	"learn.check",
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

// removedKey is a key recall dropped and the text that replaces it.
type removedKey struct {
	pattern string // dotted, * matches one segment; matches the key or a table above it
	text    string
	// forValue, when set, builds the text from the key's value.
	forValue func(v any) string
}

// removedKeys starts each row when a release removes a key.
var removedKeys = []removedKey{
	{pattern: "learn.ledger", text: "the rule ledger is always ~/.local/state/recall/ledger.json; move a ledger kept elsewhere there and delete the key"},
	{pattern: "learn.candidates", text: "the candidate ledger is always ~/.local/state/recall/learn/candidates.json; move one kept elsewhere there and delete the key"},
	{pattern: "recur.prefix", forValue: func(v any) string {
		p, _ := v.(string)
		return recurPrefixCleanup(p, runtime.GOOS)
	}},
	{pattern: "archive-chats.price_per_gb", text: "the estimate always uses S3 Standard's $0.023/GB-month; delete the key"},
	{pattern: "learn.require_owner_evidence", text: "the owner-evidence check is always on; delete the key"},
	{pattern: "learn.window_days", text: "pass recall propose --window, like --window 365d or --window all; delete the key"},
	{pattern: "archive-chats.lookback", text: "each archive run checks 4 weeks; pass recall archive --lookback N for more, and delete the key"},
	{pattern: "store.s3.bucket_prefix", text: "set the full name in store.s3.bucket, like recall config set store.s3.bucket chat-archive-<account id>-<region>-an, and delete the key"},
}

// recurPrefixCleanup says how to remove the jobs an old custom prefix installed.
func recurPrefixCleanup(prefix, goos string) string {
	s := "the scheduler marker is fixed at " + RecurPrefix + "; delete the key"
	switch {
	case prefix == "" || prefix == RecurPrefix:
		return s
	case !SchedulerName.MatchString(prefix):
		return s + "; recall refused this prefix, so it never installed a job"
	case strings.HasPrefix(RecurPrefix+".", prefix+".") || strings.HasPrefix(prefix+".", RecurPrefix+"."):
		return s + "; this prefix overlaps " + RecurPrefix + ", so a pattern for its jobs would match recall's own, and its jobs need removing by hand"
	}
	s += ", then remove the jobs marked " + prefix + " before running recall recur sync"
	if goos == "darwin" {
		return s + `: for f in ~/Library/LaunchAgents/` + prefix + `.*.plist; do launchctl bootout gui/$(id -u) "$f"; rm "$f"; done`
	}
	re := strings.ReplaceAll(prefix, ".", `\.`)
	return s + `: crontab -l | sed '/^# BEGIN recall-recur ` + re + ` /,/^# END recall-recur ` + re + `$/d' | crontab -`
}

// Warning is a key the schema doesn't know, which the load ignored.
type Warning struct {
	Key    string
	Origin Origin
	// Replacement says what replaces a removed key; empty when recall knows none.
	Replacement string
	segs        []string
	table       bool // an empty table, not a leaf
}

func (w Warning) String() string {
	what := "key"
	if w.table {
		what = "table"
	}
	s := fmt.Sprintf("%s: unknown %s %q, ignored", w.Origin, what, w.Key)
	if w.Replacement != "" {
		s += "; " + w.Replacement
	}
	return s
}

func allowed(key string) bool {
	for _, pattern := range schema {
		if matches(pattern, key) {
			return true
		}
	}
	return false
}

// replacement returns the removed-key text for key, set to v, if any.
func replacement(key string, v any) string {
	ks := strings.Split(key, ".")
	for _, r := range removedKeys {
		ps := strings.Split(r.pattern, ".")
		if len(ps) <= len(ks) && matches(r.pattern, strings.Join(ks[:len(ps)], ".")) {
			if r.forValue != nil {
				return r.forValue(v)
			}
			return r.text
		}
	}
	return ""
}

// wrongShape reports a known leaf set to a table, or a known table set to
// a value.
func wrongShape(key string) string {
	ks := strings.Split(key, ".")
	for i := 1; i < len(ks); i++ {
		if allowed(strings.Join(ks[:i], ".")) {
			return fmt.Sprintf("%q is a value, not a table", strings.Join(ks[:i], "."))
		}
	}
	for _, pattern := range schema {
		ps := strings.Split(pattern, ".")
		if len(ps) > len(ks) && matches(strings.Join(ps[:len(ks)], "."), key) {
			return fmt.Sprintf("%q is a table, not a value", key)
		}
	}
	return ""
}

// knownTable reports a table path some schema key sits under.
func knownTable(key string) bool {
	ks := strings.Split(key, ".")
	for _, pattern := range schema {
		ps := strings.Split(pattern, ".")
		if len(ps) > len(ks) && matches(strings.Join(ps[:len(ks)], "."), key) {
			return true
		}
	}
	return false
}

// node is one merged leaf, or one empty table, by its key segments.
type node struct {
	segs  []string
	table bool
}

func walk(m map[string]any, prefix []string, out *[]node) {
	for k, v := range m {
		segs := append(append([]string{}, prefix...), k)
		sub, ok := v.(map[string]any)
		switch {
		case !ok:
			*out = append(*out, node{segs: segs})
		case len(sub) == 0:
			*out = append(*out, node{segs: segs, table: true})
		default:
			walk(sub, segs, out)
		}
	}
}

// validate returns a warning for each merged leaf key, or empty table,
// the schema doesn't know; the removed encryption keys and a known key in
// the wrong shape are errors, naming the file and line that set them.
func (c *Config) validate() ([]Warning, error) {
	var nodes []node
	walk(c.values, nil, &nodes)
	sort.Slice(nodes, func(i, j int) bool {
		return strings.Join(nodes[i].segs, ".") < strings.Join(nodes[j].segs, ".")
	})
	var warnings []Warning
	for _, n := range nodes {
		key := strings.Join(n.segs, ".")
		origin := c.origin[key]
		if n.table {
			origin = c.tables[key]
		}
		if removedEncryptKey(key) {
			return nil, fmt.Errorf("%s: %q sets client-side encryption of the archive, a feature recall removed; the bucket's own encryption and access policy protect the store, so delete the key", origin, key)
		}
		if n.table && knownTable(key) {
			continue
		}
		if !n.table && allowed(key) {
			continue
		}
		msg := wrongShape(key)
		if n.table && allowed(key) {
			msg = fmt.Sprintf("%q is a value, not a table", key)
		}
		if msg != "" {
			return nil, fmt.Errorf("%s: %s", origin, msg)
		}
		value, _ := c.Get(key)
		warnings = append(warnings, Warning{Key: key, Origin: origin, Replacement: replacement(key, value), segs: n.segs, table: n.table})
	}
	return warnings, nil
}

// drop removes each warned key from the merged values, and any table it
// leaves empty, so Get, Leaves, and Settings never see it.
func (c *Config) drop(warnings []Warning) {
	for _, w := range warnings {
		delete(c.origin, w.Key)
		parts := w.segs
		maps := []map[string]any{c.values}
		for _, p := range parts[:len(parts)-1] {
			m, ok := maps[len(maps)-1][p].(map[string]any)
			if !ok {
				break
			}
			maps = append(maps, m)
		}
		if len(maps) != len(parts) {
			continue
		}
		delete(maps[len(maps)-1], parts[len(parts)-1])
		delete(c.tables, w.Key)
		for i := len(maps) - 1; i > 0 && len(maps[i]) == 0; i-- {
			delete(maps[i-1], parts[i-1])
			delete(c.tables, strings.Join(parts[:i], "."))
		}
	}
}

// Warnings lists the keys the load ignored, in file order.
func (c *Config) Warnings() []Warning {
	return c.warnings
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
	// Targets and Bin are the keep skill's, read through config get; the
	// binary only reports their defaults.
	Targets []string
	Bin     string
}

// DefaultJobEnabled is a recur job's enabled when the job doesn't set it.
const DefaultJobEnabled = true

// LearnSettings configure the learning loop (docs/learning.md).
type LearnSettings struct {
	// Runner is the harness CLI model calls go through.
	Runner string
	// MapModel reads one session; ReduceModel folds a window into proposals.
	MapModel    string
	ReduceModel string
	// MapEffort, ReduceEffort, and ApplyEffort are the effort level each
	// step's model runs at, one of the runner's Efforts; empty passes
	// none, and the harness default applies. Replays run at MapEffort.
	MapEffort    string
	ReduceEffort string
	ApplyEffort  string
	// Targets are the context files rules live in.
	Targets []string
	// Repos are the checkouts whose own context files a repo-only lesson
	// may land in.
	Repos []string
	// RepoNotes is a directory inside a private git checkout that holds a
	// file of rules for each repo recall doesn't change; empty is off.
	RepoNotes string
	// RetireDays: a rule with no relevance for this long is proposed for
	// retirement.
	RetireDays int
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
	Endpoint      string
	RetentionDays int
}

type ArchiveSettings struct {
	// Redact replaces secret-shaped tokens in a week's text files as its
	// tarball is built.
	Redact bool
}

type HarnessSettings struct {
	Source  string
	Enabled bool
	// Format is the known harness whose transcript format a harness
	// recall doesn't know shares, or FormatEvents; empty for a known one.
	Format string
	// Import is the plugin that fetches an events harness's sessions
	// (docs/import.md); nil when none is configured.
	Import *ImportSettings
}

// ImportSettings name the plugin an events harness imports through.
type ImportSettings struct {
	// Command is the program and its arguments, run without a shell.
	Command []string
	// Timeout bounds one run of the plugin.
	Timeout time.Duration
}

// DefaultImportTimeout bounds one import run when import.timeout is unset.
const DefaultImportTimeout = 10 * time.Minute

type RecurSettings struct {
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
			S3:   S3Settings{RetentionDays: 3650},
		},
		Harnesses: map[string]HarnessSettings{},
		Keep:      KeepSettings{Bin: filepath.Join(home, "bin")},
		Recur:     RecurSettings{Jobs: map[string]Job{}},
		// The credential package's DefaultTimeout.
		Review: ReviewSettings{Credentials: CredentialSettings{Timeout: 30 * time.Second}},
		// The runner, models, targets, skills, and evidence trailer follow
		// the enabled harnesses, so they are filled in below.
		Learn: LearnSettings{
			RetireDays: 90,
			MapWorkers: 4,
			Bin:        filepath.Join(home, "bin"),
			Review:     "pr",
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
	str("store.s3.endpoint", &s.Store.S3.Endpoint)
	if err == nil && s.Store.Kind == "s3" && s.Store.S3.Bucket == "" {
		o, _ := c.Origin("store.kind")
		err = fmt.Errorf("%s: store.kind is \"s3\" and store.s3.bucket is unset; name the bucket with recall config set store.s3.bucket <name>", o)
	}
	num("store.s3.retention_days", &s.Store.S3.RetentionDays)
	c.boolKey("archive-chats.redact", &s.Archive.Redact, &err)
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
	str("learn.map_effort", &s.Learn.MapEffort)
	str("learn.reduce_effort", &s.Learn.ReduceEffort)
	str("learn.apply_effort", &s.Learn.ApplyEffort)
	path("learn.repo_notes", &s.Learn.RepoNotes)
	num("learn.retire_days", &s.Learn.RetireDays)
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
	path("keep.bin", &s.Keep.Bin)
	if v, ok := c.Get("keep.targets"); ok && err == nil {
		list, lok := v.([]any)
		if !lok {
			err = c.typeErr("keep.targets", "an array of strings", v)
		} else {
			for _, e := range list {
				ev, sok := e.(string)
				if !sok {
					err = c.typeErr("keep.targets", "an array of strings", e)
					break
				}
				s.Keep.Targets = append(s.Keep.Targets, ev)
			}
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
	if err == nil && s.Learn.RepoNotes != "" {
		err = c.checkRepoNotes(s)
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
		str(base+".format", &h.Format)
		if h.Format == FormatEvents && h.Source == "" {
			h.Source = filepath.Join(ImportDir(home, name), "sessions")
		}
		c.boolKey(base+".enabled", &h.Enabled, &err)
		if err == nil {
			err = c.checkFormat(name, h)
		}
		if err == nil {
			h.Import, err = c.importSettings(name, h, home)
		}
		if h.Enabled {
			s.Harnesses[name] = h
		}
	}
	if _, declared := c.Get("harness." + HarnessClaude); !declared {
		s.Harnesses[HarnessClaude] = HarnessSettings{Source: DefaultSource(HarnessClaude, home), Enabled: true}
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
	if _, set := c.Get("keep.targets"); !set {
		s.Keep.Targets = slices.Clone(s.Learn.Targets)
	}
	// The loop runs through the primary harness, lands graduated skills
	// where that harness loads them, and asks the runner's CLI for the
	// models that CLI knows. A runner recall has no defaults for gets the
	// first known harness's.
	ifUnset := func(key string, dst *string, def string) {
		if _, set := c.Get(key); !set {
			*dst = def
		}
	}
	primary := PrimaryHarness(s.Harnesses)
	ifUnset("learn.runner", &s.Learn.Runner, primary)
	ifUnset("learn.skills", &s.Learn.Skills, DefaultSkills(primary, home))
	ifUnset("learn.evidence_trailer", &s.Learn.EvidenceTrailer, DefaultTrailer(primary))
	runner, ok := defaultsFor(s.Learn.Runner)
	if !ok {
		runner = harnesses[0]
	}
	ifUnset("learn.map_model", &s.Learn.MapModel, runner.mapModel)
	ifUnset("learn.reduce_model", &s.Learn.ReduceModel, runner.reduceModel)
	ifUnset("learn.apply_model", &s.Learn.ApplyModel, runner.applyModel)
	for _, e := range []struct{ key, level string }{
		{"learn.map_effort", s.Learn.MapEffort}, {"learn.reduce_effort", s.Learn.ReduceEffort}, {"learn.apply_effort", s.Learn.ApplyEffort},
	} {
		if levels := Efforts(s.Learn.Runner); err == nil && e.level != "" && !slices.Contains(levels, e.level) {
			o, _ := c.Origin(e.key)
			err = fmt.Errorf("%s: %s %q is not an effort level the %s runner takes; want one of %s, or leave it unset for the harness default",
				o, e.key, e.level, s.Learn.Runner, strings.Join(levels, ", "))
		}
	}
	for _, name := range c.tableNames("recur.jobs") {
		j := Job{Name: name}
		enabled := DefaultJobEnabled
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

// importSettings reads harness.<name>.import: a command, required, and a
// timeout, on an events harness only.
func (c *Config) importSettings(name string, h HarnessSettings, home string) (*ImportSettings, error) {
	key := "harness." + name + ".import"
	v, ok := c.Get(key)
	if !ok {
		return nil, nil
	}
	o, _ := c.TableOrigin(key)
	if _, table := v.(map[string]any); !table {
		o, _ = c.Origin(key)
		return nil, fmt.Errorf("%s: %s is a table: [%s] with a command", o, key, key)
	}
	if h.Format != FormatEvents {
		return nil, fmt.Errorf("%s: [%s] needs harness.%s.format = %q; an import writes recall's own events", o, key, name, FormatEvents)
	}
	imp := &ImportSettings{Timeout: DefaultImportTimeout}
	cv, ok := c.Get(key + ".command")
	if !ok {
		return nil, fmt.Errorf("%s: [%s] has no command; set command to the plugin to run, then its arguments", o, key)
	}
	list, ok := cv.([]any)
	if !ok {
		return nil, c.typeErr(key+".command", "an array of strings: the program, then its arguments", cv)
	}
	for i, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, c.typeErr(key+".command", "an array of strings", e)
		}
		if i == 0 {
			s = ExpandHome(s, home)
		}
		imp.Command = append(imp.Command, s)
	}
	if len(imp.Command) == 0 || imp.Command[0] == "" {
		co, _ := c.Origin(key + ".command")
		return nil, fmt.Errorf("%s: %s.command names no program", co, key)
	}
	if tv, ok := c.Get(key + ".timeout"); ok {
		to, _ := c.Origin(key + ".timeout")
		s, sok := tv.(string)
		d, perr := time.ParseDuration(s)
		if !sok {
			return nil, c.typeErr(key+".timeout", "a duration string like \"10m\"", tv)
		}
		if perr != nil || d <= 0 {
			return nil, fmt.Errorf("%s: %s.timeout %q isn't a positive duration like \"10m\"", to, key, s)
		}
		imp.Timeout = d
	}
	return imp, nil
}

// checkFormat rejects a format on a harness recall knows, a format that
// isn't a known harness or events, a name recall uses as a store prefix of
// its own, and a formatted harness with no source.
func (c *Config) checkFormat(name string, h HarnessSettings) error {
	if h.Format == "" {
		return nil
	}
	key := "harness." + name + ".format"
	o, _ := c.Origin(key)
	if _, known := defaultsFor(name); known {
		return fmt.Errorf("%s: %s is a harness recall knows, so %s can't change its format", o, name, key)
	}
	if _, known := defaultsFor(h.Format); !known && h.Format != FormatEvents {
		return fmt.Errorf("%s: %s is %q; it names the transcript format this harness shares: %s", o, key, h.Format, strings.Join(Formats, ", "))
	}
	if reservedHarness(name) {
		return fmt.Errorf("%s: harness %q is a store prefix recall uses for itself; name it something else", o, name)
	}
	if h.Source == "" {
		return fmt.Errorf("%s: harness %q has a format but no source", o, name)
	}
	return nil
}

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

// checkRepoNotes refuses a repo notes directory under a keep.public path,
// since repo notes describe other people's repos.
func (c *Config) checkRepoNotes(s *Settings) error {
	o, _ := c.Origin("learn.repo_notes")
	dir := resolved(s.Learn.RepoNotes)
	for _, pub := range s.Keep.Public {
		if inside(dir, resolved(pub)) {
			return fmt.Errorf("%s: learn.repo_notes %s is under keep.public %s; repo notes describe other people's repos and stay private", o, s.Learn.RepoNotes, pub)
		}
	}
	return nil
}

// resolved is path with symlinks resolved as far as it exists, so a
// symlink can't hide that one path is under another.
func resolved(path string) string {
	path = filepath.Clean(path)
	var rest []string
	for {
		if r, err := filepath.EvalSymlinks(path); err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(append([]string{path}, rest...)...)
		}
		rest = append([]string{filepath.Base(path)}, rest...)
		path = parent
	}
}

// inside reports whether path is dir or below it.
func inside(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}
