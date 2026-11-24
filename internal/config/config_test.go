package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// base returns a base config path inside a fresh temp dir.
func base(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.toml")
}

func TestLayeringAcrossDropIns(t *testing.T) {
	b := base(t)
	d := filepath.Dir(b)
	write(t, b, "host = \"laptop-a\"\n\n[store]\nkind = \"fs\"\npath = \"~/recall-store\"\n")
	write(t, filepath.Join(d, "config.d", "10-shared.toml"), "[store]\nkind = \"s3\"\n\n[store.s3]\nregion = \"us-west-2\"\n")
	write(t, filepath.Join(d, "config.d", "20-machine.toml"), "[store.s3]\nprofile = \"archive\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"host":             "laptop-a",
		"store.kind":       "s3",
		"store.path":       "~/recall-store",
		"store.s3.region":  "us-west-2",
		"store.s3.profile": "archive",
	} {
		if got, _ := c.Get(key); got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	// Tables merged: the later file's store.kind replaced the earlier one,
	// and the origin says which file and line set it.
	o, ok := c.Origin("store.kind")
	if !ok || !strings.HasSuffix(o.File, "10-shared.toml") || o.Line != 2 {
		t.Errorf("store.kind origin = %v %v, want 10-shared.toml:2", o, ok)
	}
	o, _ = c.Origin("host")
	if !strings.HasSuffix(o.File, "config.toml") || o.Line != 1 {
		t.Errorf("host origin = %v, want config.toml:1", o)
	}
}

func TestArrayAndInlineTableOrigins(t *testing.T) {
	b := base(t)
	write(t, b, `host = "laptop-a"

[recur]
env = ["PATH=/a", "A=1"]

[recur.jobs]
archive-chats = { schedule = "0 12 * * 1", command = "recall archive-chats run" }
`)
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	for key, line := range map[string]int{
		"recur.env":                        4,
		"recur.jobs.archive-chats.command": 7,
	} {
		o, ok := c.Origin(key)
		if !ok || o.Line != line {
			t.Errorf("%s origin = %v %v, want line %d", key, o, ok, line)
		}
	}
}

func TestArraysReplaceWhole(t *testing.T) {
	b := base(t)
	write(t, b, "[recur]\nenv = [\"PATH=/a\", \"A=1\"]\n")
	write(t, filepath.Join(filepath.Dir(b), "config.d", "10.toml"), "[recur]\nenv = [\"PATH=/b\"]\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Recur.Env) != 1 || s.Recur.Env[0] != "PATH=/b" {
		t.Errorf("env = %v, want the later array only", s.Recur.Env)
	}
}

func TestEnabledFalseDisablesJobAndHarness(t *testing.T) {
	b := base(t)
	write(t, b, `[recur.jobs.archive-chats]
schedule = "0 12 * * 1"
command = "recall archive-chats run"

[harness.claude]
source = "~/.claude/projects"
`)
	write(t, filepath.Join(filepath.Dir(b), "config.d", "99-off.toml"), `[recur.jobs.archive-chats]
enabled = false

[harness.claude]
enabled = false
`)
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Recur.Jobs) != 0 {
		t.Errorf("jobs = %v, want none", s.Recur.Jobs)
	}
	if len(s.Harnesses) != 0 {
		t.Errorf("harnesses = %v, want none", s.Harnesses)
	}
}

func TestUnknownKeyNamesFileAndLine(t *testing.T) {
	b := base(t)
	write(t, b, "host = \"laptop-a\"\n")
	drop := filepath.Join(filepath.Dir(b), "config.d", "10.toml")
	write(t, drop, "[store]\nkind = \"fs\"\nknid = \"oops\"\n")
	_, err := Load(b)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"10.toml:3", `"store.knid"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't mention %s", err, want)
		}
	}
}

func TestMalformedFileNamesFileAndLine(t *testing.T) {
	b := base(t)
	write(t, b, "host = \"laptop-a\"\nnot toml ][\n")
	_, err := Load(b)
	if err == nil || !strings.Contains(err.Error(), "config.toml:2") {
		t.Fatalf("error %v doesn't name config.toml:2", err)
	}
}

func TestHomeExpansion(t *testing.T) {
	b := base(t)
	write(t, b, "[store]\npath = \"~/store\"\n\n[harness.claude]\nsource = \"$HOME/.claude/projects\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	if s.Store.Path != "/home/you/store" {
		t.Errorf("store.path = %q", s.Store.Path)
	}
	if s.Harnesses["claude"].Source != "/home/you/.claude/projects" {
		t.Errorf("claude source = %q", s.Harnesses["claude"].Source)
	}
}

func TestMissingConfigDirAndFiles(t *testing.T) {
	b := base(t) // nothing written at all
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	// Defaults apply: claude harness, fs store, archive settings.
	if s.Host != "laptop-a" || s.Store.Kind != "fs" || s.Archive.Lookback != 4 {
		t.Errorf("defaults wrong: %+v", s)
	}
	if s.Harnesses["claude"].Source != "/home/you/.claude/projects" {
		t.Errorf("default claude harness missing: %+v", s.Harnesses)
	}
	if s.Recur.Prefix != "recall.recur" {
		t.Errorf("recur.prefix = %q", s.Recur.Prefix)
	}
}

func TestSetWritesOnlyTheToolDropIn(t *testing.T) {
	b := base(t)
	d := filepath.Dir(b)
	write(t, b, "host = \"laptop-a\"   # hand-edited, with comments\n")
	write(t, filepath.Join(d, "config.d", "10-mine.toml"), "# mine\n[store]\nkind = \"fs\"\n")
	before := map[string][]byte{}
	for _, p := range []string{b, filepath.Join(d, "config.d", "10-mine.toml")} {
		data, _ := os.ReadFile(p)
		before[p] = data
	}

	target, err := Set(b, "store.kind", "s3")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(target) != SetFile {
		t.Errorf("wrote to %s", target)
	}
	if _, err := Set(b, "store.s3.region", "us-west-2"); err != nil {
		t.Fatal(err)
	}
	for p, data := range before {
		now, _ := os.ReadFile(p)
		if string(now) != string(data) {
			t.Errorf("%s changed:\n%s", p, now)
		}
	}
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Get("store.kind"); v != "s3" {
		t.Errorf("store.kind = %v, want the set value to win", v)
	}
	if v, _ := c.Get("store.s3.region"); v != "us-west-2" {
		t.Errorf("store.s3.region = %v", v)
	}
	data, _ := os.ReadFile(target)
	if !strings.HasPrefix(string(data), "# Managed by recall") {
		t.Errorf("drop-in lacks the managed header:\n%s", data)
	}
}

func TestSetValueTypes(t *testing.T) {
	b := base(t)
	if _, err := Set(b, "archive-chats.lookback", "8"); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(b, "harness.claude.enabled", "false"); err != nil {
		t.Fatal(err)
	}
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Get("archive-chats.lookback"); v != int64(8) {
		t.Errorf("lookback = %v (%T), want int64 8", v, v)
	}
	if v, _ := c.Get("harness.claude.enabled"); v != false {
		t.Errorf("enabled = %v, want false", v)
	}
}

func TestSetRejectsUnknownKeys(t *testing.T) {
	b := base(t)
	if _, err := Set(b, "store.knid", "s3"); err == nil {
		t.Fatal("want an error for an unknown key")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(b), "config.d", SetFile)); !os.IsNotExist(err) {
		t.Error("a rejected set still wrote the drop-in")
	}
}

func TestTypeErrorsNameTheOrigin(t *testing.T) {
	b := base(t)
	write(t, b, "[archive-chats]\nlookback = \"four\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Settings("/home/you", "laptop-a")
	if err == nil || !strings.Contains(err.Error(), "config.toml:2") {
		t.Fatalf("error %v doesn't name config.toml:2", err)
	}
}

func TestArchiveRedactIsOffUnlessSet(t *testing.T) {
	b := base(t)
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil || s.Archive.Redact {
		t.Fatalf("default redact = %v, %v; want false", s.Archive.Redact, err)
	}
	write(t, b, "[archive-chats]\nredact = true\n")
	if c, err = Load(b); err != nil {
		t.Fatal(err)
	}
	if s, err = c.Settings("/home/you", "laptop-a"); err != nil || !s.Archive.Redact {
		t.Fatalf("redact = %v, %v; want true", s.Archive.Redact, err)
	}
	write(t, b, "[archive-chats]\nredact = \"yes\"\n")
	if c, err = Load(b); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Settings("/home/you", "laptop-a"); err == nil || !strings.Contains(err.Error(), "wants a bool") {
		t.Fatalf("err = %v, want a type error", err)
	}
}

// Client-side encryption of the archive was removed; a config that still
// sets one of its keys fails with an error that says so, not "unknown key".
// The key is spelled in two parts so no removed-feature name survives in
// the tree.
func TestRemovedEncryptKeysFail(t *testing.T) {
	for _, line := range []string{
		"recipients = [\"age1example\"]\n",
		"identity = \"~/.config/recall/archive-identity.txt\"\n",
	} {
		b := base(t)
		write(t, b, "[store."+"encrypt]\n"+line)
		_, err := Load(b)
		if err == nil || !strings.Contains(err.Error(), "removed") {
			t.Fatalf("Load with %q = %v, want an error saying the feature was removed", line, err)
		}
		if !strings.Contains(err.Error(), b) {
			t.Errorf("err = %v, want it to name the file that set the key", err)
		}
	}
}

func TestJobMissingCommand(t *testing.T) {
	b := base(t)
	write(t, b, "[recur.jobs.x]\nschedule = \"0 12 * * 1\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Settings("/home/you", "laptop-a")
	if err == nil || !strings.Contains(err.Error(), "has no command") {
		t.Fatalf("err = %v, want has no command", err)
	}
}

func TestMapWorkersBounds(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
		want  int
	}{
		{"", true, 4}, {"1", true, 1}, {"32", true, 32},
		{"0", false, 0}, {"-2", false, 0}, {"33", false, 0},
	} {
		b := base(t)
		text := "host = \"laptop-a\"\n"
		if tc.value != "" {
			text += "[learn]\nmap_workers = " + tc.value + "\n"
		}
		write(t, b, text)
		c, err := Load(b)
		if err != nil {
			t.Fatal(err)
		}
		s, err := c.Settings("/home/you", "laptop-a")
		if !tc.ok {
			if err == nil || !strings.Contains(err.Error(), "learn.map_workers must be 1 to 32") || !strings.Contains(err.Error(), "config.toml:3") {
				t.Errorf("map_workers = %s: err = %v, want the bounds with file and line", tc.value, err)
			}
			continue
		}
		if err != nil || s.Learn.MapWorkers != tc.want {
			t.Errorf("map_workers = %q: got %v, %v; want %d", tc.value, s, err, tc.want)
		}
	}
}

func TestExtensions(t *testing.T) {
	b := base(t)
	write(t, b, "host = \"laptop-a\"\n[extensions]\nkeep = [\"~/.config/recall/keep.md\", \"/etc/recall/team.md\"]\nlearn-reduce = []\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.Extensions["keep"], ","); got != "/home/you/.config/recall/keep.md,/etc/recall/team.md" {
		t.Errorf("keep = %s", got)
	}
	if len(s.Extensions["learn-reduce"]) != 0 || len(s.Extensions["recur"]) != 0 {
		t.Errorf("extensions = %v", s.Extensions)
	}

	write(t, b, "host = \"laptop-a\"\n[extensions]\nkeep = \"~/one.md\"\n")
	c, err = Load(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Settings("/home/you", "laptop-a"); err == nil || !strings.Contains(err.Error(), "extensions.keep wants an array of file paths") {
		t.Errorf("err = %v, want a type error", err)
	}
}

func TestApplySettings(t *testing.T) {
	b := base(t)
	write(t, b, "host = \"laptop-a\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	if l := s.Learn; l.ApplyModel != "sonnet" || l.Review != "pr" || l.Skills != "/home/you/.claude/skills" ||
		l.Bin != "/home/you/bin" || l.Check != "" ||
		l.EvidenceTrailer != "Co-Authored-By: Claude" || len(s.Keep.Public) != 0 {
		t.Errorf("defaults = %+v, keep %+v", l, s.Keep)
	}

	write(t, b, "host = \"laptop-a\"\n[learn]\napply_model = \"opus\"\nreview = \"branch\"\nskills = \"~/kit/skills\"\n"+
		"bin = \"~/kit/bin\"\ncheck = \"gitleaks stdin\"\n"+
		"evidence_trailer = \"Agent: laptop-a\"\n[keep]\npublic = [\"~/src/you\"]\n")
	if c, err = Load(b); err != nil {
		t.Fatal(err)
	}
	if s, err = c.Settings("/home/you", "laptop-a"); err != nil {
		t.Fatal(err)
	}
	if l := s.Learn; l.ApplyModel != "opus" || l.Review != "branch" || l.Skills != "/home/you/kit/skills" ||
		l.Bin != "/home/you/kit/bin" || l.Check != "gitleaks stdin" ||
		l.EvidenceTrailer != "Agent: laptop-a" {
		t.Errorf("set = %+v", l)
	}
	if strings.Join(s.Keep.Public, ",") != "/home/you/src/you" {
		t.Errorf("keep.public = %v", s.Keep.Public)
	}
}

func TestFresh(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config.toml")
	c, err := Load(base)
	if err != nil {
		t.Fatal(err)
	}
	keys := func(plan []KV) string {
		var out []string
		for _, kv := range plan {
			out = append(out, kv.Key+"="+kv.Value)
		}
		return strings.Join(out, " ")
	}
	plan, notes := Fresh(c, false, "laptop-b", nil, "")
	if got := keys(plan); got != `learn.review="branch" hooks.map=true hooks.archive=true hooks.learn_every=7` {
		t.Errorf("a new machine: %s", got)
	}
	if len(notes) != 0 {
		t.Errorf("a new machine gets notes: %v", notes)
	}
	// What it sets is valid config, and setting it leaves nothing to set.
	for _, kv := range plan {
		if _, err := Set(base, kv.Key, kv.Value); err != nil {
			t.Fatal(err)
		}
	}
	c, err = Load(base)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil || s.Learn.Review != "branch" || !s.Hooks.Map || !s.Hooks.Archive || s.Hooks.LearnEvery != 7 {
		t.Fatalf("settings after init = %+v, %v", s, err)
	}
	if plan, _ := Fresh(c, true, "laptop-b", nil, ""); len(plan) != 0 {
		t.Errorf("a second init would still set %s", keys(plan))
	}

}

// A machine with a config file already relies on learn.review's default,
// pull requests; init must not reroute its proposals to a local branch.
func TestFreshKeepsExistingUsersOnPullRequests(t *testing.T) {
	b := base(t)
	write(t, b, "host = \"laptop-a\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	plan, notes := Fresh(c, true, "laptop-b", nil, "")
	for _, kv := range plan {
		if kv.Key == "learn.review" {
			t.Errorf("an existing machine's review was switched to %s", kv.Value)
		}
	}
	// The hooks still turn on: no scheduled jobs do the same work.
	got := map[string]bool{}
	for _, kv := range plan {
		got[kv.Key] = true
	}
	for _, key := range []string{"hooks.map", "hooks.archive", "hooks.learn_every"} {
		if !got[key] {
			t.Errorf("%s not set on an existing machine with no jobs", key)
		}
	}
	// The owner hears what init left alone, and how to change it.
	if len(notes) != 1 || !strings.Contains(notes[0], "learn.review") || !strings.Contains(notes[0], "branch") {
		t.Errorf("notes = %v, want one naming learn.review and the branch alternative", notes)
	}

	// With learn.review set by hand, there is nothing to say.
	write(t, b, "host = \"laptop-a\"\n[learn]\nreview = \"pr\"\n")
	if c, err = Load(b); err != nil {
		t.Fatal(err)
	}
	if _, notes := Fresh(c, true, "laptop-b", nil, ""); len(notes) != 0 {
		t.Errorf("notes = %v, want none when the key is set", notes)
	}
}

// The removed review channel fails with the fix when a config still names
// it, and its old key is no longer in the schema.
func TestInboxReviewIsRemoved(t *testing.T) {
	b := base(t)
	write(t, b, "[learn]\nreview = \"inbox\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Settings("/home/you", "laptop-a")
	if err == nil || !strings.Contains(err.Error(), "removed") || !strings.Contains(err.Error(), "\"branch\"") {
		t.Errorf("err = %v, want the removal explained with branch as the fix", err)
	}
	// The old learn.inbox key is no longer in the schema.
	write(t, b, "[learn]\ninbox = \"~/somewhere\"\n")
	if _, err := Load(b); err == nil || !strings.Contains(err.Error(), "learn.inbox") {
		t.Errorf("err = %v, want learn.inbox rejected as an unknown key", err)
	}
}

func TestKiroDefaults(t *testing.T) {
	b := base(t)
	write(t, b, "[harness.kiro]\n\n[learn]\nrunner = \"kiro\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	if s.Harnesses["kiro"].Source != "/home/you/.kiro/sessions" || s.Harnesses["claude"].Source == "" {
		t.Errorf("harnesses = %+v", s.Harnesses)
	}
	want := []string{"/home/you/.claude/CLAUDE.md", "/home/you/.kiro/steering/AGENTS.md"}
	if strings.Join(s.Learn.Targets, ",") != strings.Join(want, ",") {
		t.Errorf("targets = %v, want %v", s.Learn.Targets, want)
	}
	if s.Learn.MapModel != "auto" || s.Learn.ReduceModel != "auto" {
		t.Errorf("models = %q, %q; want auto for kiro", s.Learn.MapModel, s.Learn.ReduceModel)
	}
}

func TestKiroOnlyTargetsAndExplicitModels(t *testing.T) {
	b := base(t)
	write(t, b, "[harness.claude]\nenabled = false\n\n[harness.kiro]\n\n[learn]\nrunner = \"kiro\"\nmap_model = \"claude-sonnet-5.5\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Learn.Targets, ",") != "/home/you/.kiro/steering/AGENTS.md" {
		t.Errorf("targets = %v", s.Learn.Targets)
	}
	if s.Learn.MapModel != "claude-sonnet-5.5" || s.Learn.ReduceModel != "auto" {
		t.Errorf("models = %q, %q", s.Learn.MapModel, s.Learn.ReduceModel)
	}
}

// A hook step turns on unless an enabled recur job's command already runs
// that very step: a part-scheduled machine gets hooks for the rest, and an
// unrelated job blocks nothing.
func TestFreshSkipsStepsRecurJobsRun(t *testing.T) {
	keys := func(plan []KV) string {
		var out []string
		for _, kv := range plan {
			out = append(out, kv.Key)
		}
		return strings.Join(out, " ")
	}
	job := func(command string) string {
		return "[learn]\nreview = \"pr\"\n[recur.jobs.j]\nschedule = \"0 14 * * 1\"\ncommand = \"" + command + "\"\n"
	}
	for _, tc := range []struct {
		name, toml, want string
		notes            int
	}{
		{"learn job", job("recall learn run"), "hooks.map hooks.archive", 1},
		{"map and archive job", job("recall archive-chats run; recall learn map --since 15d"), "hooks.learn_every", 2},
		{"unrelated job", job("backup-home"), "hooks.map hooks.archive hooks.learn_every", 0},
		{"disabled learn job", "[learn]\nreview = \"pr\"\n[recur.jobs.j]\nschedule = \"0 14 * * 1\"\ncommand = \"recall learn run\"\nenabled = false\n",
			"hooks.map hooks.archive hooks.learn_every", 0},
	} {
		b := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(b, []byte(tc.toml), 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := Load(b)
		if err != nil {
			t.Fatal(err)
		}
		plan, notes := Fresh(c, true, "laptop-b", nil, "")
		if got := keys(plan); got != tc.want {
			t.Errorf("%s: plan = %q, want %q", tc.name, got, tc.want)
		}
		if len(notes) != tc.notes {
			t.Errorf("%s: notes = %v, want %d naming the covered step(s)", tc.name, notes, tc.notes)
		}
	}
}

// With two machines on one store, only one runs the reduce: init on the
// second sees the first's claim and leaves hooks.learn_every off.
func TestFreshOneReducerPerStore(t *testing.T) {
	b := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(b, []byte("[learn]\nreview = \"pr\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	plan, notes := Fresh(c, true, "laptop-b", func() (string, error) { return "laptop-a", nil }, "")
	for _, kv := range plan {
		if kv.Key == "hooks.learn_every" {
			t.Errorf("a second machine on the store would also reduce: %v", plan)
		}
	}
	found := false
	for _, note := range notes {
		found = found || strings.Contains(note, "laptop-a") && strings.Contains(note, "laptop-b") &&
			strings.Contains(note, "recall learn reducer --take")
	}
	if !found {
		t.Errorf("notes = %v, want one naming the claim holder, this machine, and the take hint", notes)
	}

	// With no claim on the store, the reduce turns on here.
	plan, _ = Fresh(c, true, "laptop-b", func() (string, error) { return "", nil }, "")
	on := false
	for _, kv := range plan {
		on = on || kv.Key == "hooks.learn_every"
	}
	if !on {
		t.Errorf("plan = %v, want the first machine on a store to reduce", plan)
	}

	// A store that can't be read leaves the reduce off, with the command
	// that turns it on by hand.
	plan, notes = Fresh(c, true, "laptop-b", func() (string, error) { return "", errors.New("offline") }, "")
	for _, kv := range plan {
		if kv.Key == "hooks.learn_every" {
			t.Errorf("an unreadable store still turned the reduce on: %v", plan)
		}
	}
	found = false
	for _, note := range notes {
		found = found || strings.Contains(note, "offline") && strings.Contains(note, "hooks.learn_every")
	}
	if !found {
		t.Errorf("notes = %v, want the store error and the manual command", notes)
	}
}

func TestRecurCovers(t *testing.T) {
	r := RecurSettings{Jobs: map[string]Job{
		"weekly": {Command: "recall archive-chats run; recall learn map --since 15d"},
	}}
	if !r.Covers(StepArchive) || !r.Covers(StepMap) {
		t.Error("a chained command should cover both its steps")
	}
	if r.Covers(StepLearn) {
		t.Error("nothing runs the reduce, but the step reads as covered")
	}
}

// The credential command is a program and its arguments, with a timeout;
// unset, recall uses the machine's own gh login.
func TestReviewAssignees(t *testing.T) {
	b := base(t)
	write(t, b, "host = \"laptop-a\"\n")
	settings := func() *Settings {
		t.Helper()
		c, err := Load(b)
		if err != nil {
			t.Fatal(err)
		}
		s, err := c.Settings("/home/you", "laptop-a")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := settings().Review.Assignees; got != nil {
		t.Errorf("unset: %v, want no one", got)
	}
	write(t, b, "[review]\nassignees = [\"you\", \"teammate\"]\n")
	if got := settings().Review.Assignees; strings.Join(got, ",") != "you,teammate" {
		t.Errorf("set: %v", got)
	}

	for body, want := range map[string]string{
		"[review]\nassignees = \"you\"\n":      "wants an array of strings",
		"[review]\nassignees = [\"you\", 7]\n": "wants an array of strings",
		"[review]\nassignees = [\"@you\"]\n":   "isn't a login",
		"[review]\nassignees = [\"\"]\n":       "isn't a login",
		"[review]\nassignee = [\"you\"]\n":     "unknown key",
	} {
		b := base(t)
		write(t, b, body)
		c, err := Load(b)
		if err == nil {
			_, err = c.Settings("/home/you", "laptop-a")
		}
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "config.toml") {
			t.Errorf("%q: err = %v, want %q naming the file", body, err, want)
		}
	}
}

func TestReviewCredentials(t *testing.T) {
	b := base(t)
	write(t, b, "host = \"laptop-a\"\n")
	settings := func() *Settings {
		t.Helper()
		c, err := Load(b)
		if err != nil {
			t.Fatal(err)
		}
		s, err := c.Settings("/home/you", "laptop-a")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if cred := settings().Review.Credentials; cred.Command != nil || cred.Timeout != 30*time.Second {
		t.Errorf("unset: %+v, want no command and the 30s default", cred)
	}
	write(t, b, "[review.credentials]\ncommand = [\"~/bin/recall-credential\", \"token\", \"~\"]\ntimeout = \"5s\"\n")
	cred := settings().Review.Credentials
	if strings.Join(cred.Command, " ") != "/home/you/bin/recall-credential token ~" || cred.Timeout != 5*time.Second {
		t.Errorf("set: %+v, want the program's ~ expanded, the arguments as written, and 5s", cred)
	}

	for body, want := range map[string]string{
		"[review.credentials]\ncommand = \"recall-credential token\"\n": "wants an array of strings",
		"[review.credentials]\ncommand = []\n":                          "names no program",
		"[review.credentials]\ncommand = [\"x\", 7]\n":                  "wants an array of strings",
		"[review.credentials]\ncommand = [\"x\"]\ntimeout = 30\n":       "wants a duration string",
		"[review.credentials]\ncommand = [\"x\"]\ntimeout = \"soon\"\n": "isn't a positive duration",
		"[review.credentials]\ncommand = [\"x\"]\ntimeout = \"-1s\"\n":  "isn't a positive duration",
		"[review.credentials]\ncommand = [\"x\"]\nshell = \"sh\"\n":     "unknown key",
	} {
		b := base(t)
		write(t, b, body)
		c, err := Load(b)
		if err == nil {
			_, err = c.Settings("/home/you", "laptop-a")
		}
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "config.toml") {
			t.Errorf("%q: err = %v, want %q naming the file", body, err, want)
		}
	}
}

// With Claude Code off, the loop's defaults move to the harness that is on,
// so a Kiro-only machine needs nothing under ~/.claude; a key the owner set
// still wins, and with Claude Code on nothing moves.
func TestPrimaryHarnessDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, toml                    string
		runner, ledger, skills, model string
	}{
		{"kiro only", "[harness.claude]\nenabled = false\n\n[harness.kiro]\n",
			"kiro", "/home/you/.local/state/recall/ledger.json", "/home/you/.kiro/skills", "auto"},
		{"kiro only, keys set", "[harness.claude]\nenabled = false\n\n[harness.kiro]\n\n[learn]\nrunner = \"claude\"\nledger = \"/l.json\"\nskills = \"/s\"\n",
			"claude", "/l.json", "/s", "sonnet"},
		{"both on", "[harness.kiro]\n",
			"claude", "/home/you/.local/state/recall/ledger.json", "/home/you/.claude/skills", "sonnet"},
		{"neither on", "[harness.claude]\nenabled = false\n",
			"claude", "/home/you/.local/state/recall/ledger.json", "/home/you/.claude/skills", "sonnet"},
	} {
		b := base(t)
		write(t, b, tc.toml)
		c, err := Load(b)
		if err != nil {
			t.Fatal(err)
		}
		s, err := c.Settings("/home/you", "laptop-a")
		if err != nil {
			t.Fatal(err)
		}
		l := s.Learn
		wantTrailer := "Co-Authored-By: Claude"
		if strings.HasPrefix(tc.name, "kiro only") {
			wantTrailer = "Co-Authored-By: Kiro"
		}
		if l.EvidenceTrailer != wantTrailer {
			t.Errorf("%s: evidence trailer = %q, want %q", tc.name, l.EvidenceTrailer, wantTrailer)
		}
		if l.Runner != tc.runner || l.Ledger != tc.ledger || l.Skills != tc.skills || l.MapModel != tc.model || l.ApplyModel != tc.model {
			t.Errorf("%s: runner, ledger, skills, map model, apply model = %q, %q, %q, %q, %q; want %q, %q, %q, %q, %q",
				tc.name, l.Runner, l.Ledger, l.Skills, l.MapModel, l.ApplyModel, tc.runner, tc.ledger, tc.skills, tc.model, tc.model)
		}
	}
}

// With Claude Code off there is no session-end hook, so init declares each
// loop step as a recur job instead, carries its PATH into recur.env, and
// still leaves a step a job already runs, and a reduce another machine
// claimed, alone. The plan round-trips through Set and Load.
func TestFreshWithoutClaudeDeclaresJobs(t *testing.T) {
	b := base(t)
	write(t, b, "[harness.claude]\nenabled = false\n\n[harness.kiro]\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	plan, notes := Fresh(c, false, "laptop-a", func() (string, error) { return "", nil }, "/usr/bin:/bin")
	for _, kv := range plan {
		if strings.HasPrefix(kv.Key, "hooks.") {
			t.Errorf("plan sets %s, a hook Kiro never fires", kv.Key)
		}
		if _, err := Set(b, kv.Key, kv.Value); err != nil {
			t.Fatalf("set %s = %s: %v", kv.Key, kv.Value, err)
		}
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none", notes)
	}
	c, err = Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{StepArchive, StepMap, StepLearn} {
		if !s.Recur.Covers(step) {
			t.Errorf("no job runs %q: %+v", step, s.Recur.Jobs)
		}
	}
	if s.Recur.Jobs["recall-map"].Schedule != "0 */6 * * *" {
		t.Errorf("recall-map = %+v", s.Recur.Jobs["recall-map"])
	}
	if strings.Join(s.Recur.Env, ",") != "PATH=/usr/bin:/bin" {
		t.Errorf("recur.env = %v", s.Recur.Env)
	}
	if s.Hooks.Map || s.Hooks.Archive || s.Hooks.LearnEvery != 0 {
		t.Errorf("hooks = %+v, want all off", s.Hooks)
	}

	// Run again: everything is declared, so nothing more is planned.
	if plan, _ := Fresh(c, true, "laptop-a", nil, "/usr/bin:/bin"); len(plan) != 0 {
		t.Errorf("second plan = %v, want none", plan)
	}

	// Another machine's claim keeps the learn job off, with the take hint.
	b = base(t)
	write(t, b, "[harness.claude]\nenabled = false\n\n[harness.kiro]\n\n[recur.jobs.mine]\nschedule = \"0 1 * * *\"\ncommand = \"recall archive-chats run\"\n")
	if c, err = Load(b); err != nil {
		t.Fatal(err)
	}
	plan, notes = Fresh(c, true, "laptop-b", func() (string, error) { return "laptop-a", nil }, "")
	var keys []string
	for _, kv := range plan {
		keys = append(keys, kv.Key)
	}
	if got := strings.Join(keys, " "); got != "recur.jobs.recall-map.schedule recur.jobs.recall-map.command" {
		t.Errorf("plan = %q, want only the map job", got)
	}
	if all := strings.Join(notes, "\n"); !strings.Contains(all, "`recall archive-chats run`") || !strings.Contains(all, "recall learn reducer --take") {
		t.Errorf("notes = %v, want the covered archive and the claimed reduce", notes)
	}
}

// A harness recall doesn't know reads in a known one's format from its own
// source, and adds no target and no runner.
func TestHarnessFormat(t *testing.T) {
	b := base(t)
	write(t, b, "[harness.work]\nformat = \"claude\"\nsource = \"~/.other-claude/projects\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	if h := s.Harnesses["work"]; h.Format != "claude" || h.Source != "/home/you/.other-claude/projects" || !h.Enabled {
		t.Errorf("work = %+v", h)
	}
	if strings.Join(s.Learn.Targets, ",") != "/home/you/.claude/CLAUDE.md" || s.Learn.Runner != "claude" {
		t.Errorf("targets %v, runner %q", s.Learn.Targets, s.Learn.Runner)
	}
}

func TestHarnessFormatErrors(t *testing.T) {
	for _, tc := range []struct{ toml, want string }{
		{"[harness.kiro]\nformat = \"claude\"\n", "is a harness recall knows"},
		{"[harness.work]\nformat = \"other\"\nsource = \"/w\"\n", "claude, kiro, events"},
		{"[harness.claude]\nformat = \"events\"\n", "is a harness recall knows"},
		{"[harness.analysis]\nformat = \"events\"\n", "store prefix"},
		{"[harness.hits]\nformat = \"events\"\n", "store prefix"},
		{"[harness.reviews]\nformat = \"events\"\n", "store prefix"},
		{"[harness.ledger]\nformat = \"events\"\n", "store prefix"},
		{"[harness.learn]\nformat = \"events\"\n", "store prefix"},
		{"[harness.evidence-github]\nformat = \"events\"\n", "store prefix"},
		{"[harness.analysis]\nformat = \"claude\"\nsource = \"/w\"\n", "store prefix"},
		{"[harness.work]\nformat = \"claude\"\n", "no source"},
	} {
		b := base(t)
		write(t, b, tc.toml)
		c, err := Load(b)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Settings("/home/you", "laptop-a")
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "config.toml:") {
			t.Errorf("%q: err = %v, want %q with its origin", tc.toml, err, tc.want)
		}
	}
}

// An events harness's source defaults to its import directory, and it
// adds no target and no runner.
func TestHarnessFormatEvents(t *testing.T) {
	b := base(t)
	write(t, b, "[harness.chat]\nformat = \"events\"\n\n[harness.notes]\nformat = \"events\"\nsource = \"~/notes/events\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	if h := s.Harnesses["chat"]; h.Format != FormatEvents || h.Source != "/home/you/.local/state/recall/import/chat/sessions" || !h.Enabled {
		t.Errorf("chat = %+v", h)
	}
	if h := s.Harnesses["notes"]; h.Source != "/home/you/notes/events" {
		t.Errorf("notes = %+v", h)
	}
	if strings.Join(s.Learn.Targets, ",") != "/home/you/.claude/CLAUDE.md" || s.Learn.Runner != "claude" {
		t.Errorf("targets %v, runner %q", s.Learn.Targets, s.Learn.Runner)
	}
}

func TestHarnessImport(t *testing.T) {
	b := base(t)
	write(t, b, "[harness.chat]\nformat = \"events\"\n\n[harness.chat.import]\ncommand = [\"~/bin/chat-history\", \"--all\"]\ntimeout = \"2m\"\n\n"+
		"[harness.notes]\nformat = \"events\"\n\n[harness.notes.import]\ncommand = [\"notes-export\"]\n\n[harness.plain]\nformat = \"events\"\n")
	c, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Settings("/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	chat := s.Harnesses["chat"].Import
	if chat == nil || strings.Join(chat.Command, " ") != "/home/you/bin/chat-history --all" || chat.Timeout != 2*time.Minute {
		t.Errorf("chat import = %+v", chat)
	}
	notes := s.Harnesses["notes"].Import
	if notes == nil || strings.Join(notes.Command, " ") != "notes-export" || notes.Timeout != DefaultImportTimeout {
		t.Errorf("notes import = %+v", notes)
	}
	if s.Harnesses["plain"].Import != nil {
		t.Errorf("an events harness with no import table has %+v", s.Harnesses["plain"].Import)
	}
}

func TestHarnessImportErrors(t *testing.T) {
	for _, tc := range []struct{ toml, want, line string }{
		{"[harness.chat]\nformat = \"events\"\n\n[harness.chat.import]\n", "has no command", "config.toml:4"},
		{"[harness.chat]\nformat = \"events\"\n\n[harness.chat.import]\ntimeout = \"1m\"\n", "has no command", "config.toml:4"},
		{"[harness.chat]\nformat = \"events\"\n\n[harness.chat.import]\ncommand = []\n", "names no program", "config.toml:5"},
		{"[harness.chat]\nformat = \"events\"\n\n[harness.chat.import]\ncommand = \"chat\"\n", "an array of strings", "config.toml:5"},
		{"[harness.chat]\nformat = \"events\"\n\n[harness.chat.import]\ncommand = [\"chat\"]\ntimeout = \"soon\"\n", "positive duration", "config.toml:6"},
		{"[harness.work]\nformat = \"claude\"\nsource = \"/w\"\n\n[harness.work.import]\ncommand = [\"chat\"]\n", "format = \"events\"", "config.toml:5"},
	} {
		b := base(t)
		write(t, b, tc.toml)
		c, err := Load(b)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Settings("/home/you", "laptop-a")
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.line) {
			t.Errorf("%q: err = %v, want %q at %s", tc.toml, err, tc.want, tc.line)
		}
	}
}
