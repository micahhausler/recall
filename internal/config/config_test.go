package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
