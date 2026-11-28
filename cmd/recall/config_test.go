package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/config"
)

// unknownKeyHome is a temp home whose config holds one key recall doesn't
// know, with the warning tracker reset.
func unknownKeyHome(t *testing.T, extra string) string {
	t.Helper()
	resetConfigWarnings()
	t.Cleanup(resetConfigWarnings)
	return tempHome(t, "host = \"laptop-a\"\n[store]\nkind = \"fs\"\nknid = \"oops\"\n"+extra)
}

func TestConfigWarningsGoToStderrOnly(t *testing.T) {
	unknownKeyHome(t, "")
	code, out, errOut := capture(t, "config", "get", "store.kind")
	if code != 0 || out != "fs\n" {
		t.Errorf("config get store.kind = %d, stdout %q; want 0 and one line", code, out)
	}
	if !strings.Contains(errOut, `config.toml:4: unknown key "store.knid", ignored`) {
		t.Errorf("stderr %q lacks the warning", errOut)
	}

	code, out, _ = capture(t, "config", "get", "store.knid")
	if code != 1 || out != "" {
		t.Errorf("config get of an ignored key = %d, %q; want 1 and nothing", code, out)
	}

	resetConfigWarnings()
	code, out, errOut = capture(t, "config", "show")
	if code != 0 || strings.Contains(out, "knid") || !strings.Contains(out, "store.kind") {
		t.Errorf("config show = %d, stdout %q; want the known keys only", code, out)
	}
	if !strings.Contains(errOut, `"store.knid"`) {
		t.Errorf("config show stderr %q lacks the warning", errOut)
	}
}

func TestConfigWarningsOncePerKey(t *testing.T) {
	unknownKeyHome(t, "")
	_, _, first := capture(t, "config", "get", "store.kind")
	_, _, second := capture(t, "config", "get", "host")
	if strings.Count(first, "store.knid") != 1 || strings.Contains(second, "store.knid") {
		t.Errorf("stderr %q then %q; want the warning once", first, second)
	}
}

func TestConfigNoWarningsWithoutUnknownKeys(t *testing.T) {
	resetConfigWarnings()
	tempHome(t, "host = \"laptop-a\"\n")
	if _, _, errOut := capture(t, "config", "get", "host"); errOut != "" {
		t.Errorf("stderr = %q, want nothing", errOut)
	}
}

func TestHookSessionStartWithUnknownKey(t *testing.T) {
	home := unknownKeyHome(t, "")
	upstream := filepath.Join(home, "src", "upstream")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, upstream, "init", "-q")
	gitIn(t, upstream, "remote", "add", "origin", "git@example.com:them/upstream.git")
	dir := filepath.Join(home, "notes", "repo-notes")
	note := filepath.Join(dir, "example.com", "them", "upstream.md")
	if err := os.MkdirAll(filepath.Dir(note), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(note, []byte("- Run make test.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := os.Getenv(config.EnvVar)
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("host = \"laptop-a\"\nnope = 1\n[learn]\nrepo_notes = %q\n", dir)), 0o644); err != nil {
		t.Fatal(err)
	}
	stdin := os.Stdin
	in, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(inW, `{"cwd":%q,"hook_event_name":"SessionStart"}`, upstream)
	inW.Close()
	os.Stdin = in
	code, out, errOut := capture(t, "hook", "session-start")
	os.Stdin = stdin
	var answer map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &answer) != nil || !strings.Contains(out, "Run make test.") {
		t.Errorf("session-start = %d, stdout %q; want only the JSON answer", code, out)
	}
	if !strings.Contains(errOut, `"nope"`) {
		t.Errorf("stderr %q lacks the warning", errOut)
	}
}

// showFixture is a temp home with a base config and one drop-in.
func showFixture(t *testing.T) string {
	t.Helper()
	resetConfigWarnings()
	t.Cleanup(resetConfigWarnings)
	home := tempHome(t, "host = \"laptop-a\"\n[store]\nkind = \"fs\"\n[learn]\ntargets = [\"~/a.md\", \"~/b.md\"]\n")
	if err := os.MkdirAll(filepath.Join(home, "config.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	drop := "learn.map_workers = 2\n[harness.kiro]\nenabled = false\n"
	if err := os.WriteFile(filepath.Join(home, "config.d", "50-x.local.toml"), []byte(drop), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestConfigShowUnchanged(t *testing.T) {
	showFixture(t)
	code, out, _ := capture(t, "config", "show")
	want := `harness.kiro.enabled = false          ~/config.d/50-x.local.toml:3
host = "laptop-a"                     ~/config.toml:1
learn.map_workers = 2                 ~/config.d/50-x.local.toml:1
learn.targets = ["~/a.md", "~/b.md"]  ~/config.toml:5
store.kind = "fs"                     ~/config.toml:3
`
	if code != 0 || out != want {
		t.Errorf("config show = %d\n%s\nwant\n%s", code, out, want)
	}
}

func TestConfigShowDefaults(t *testing.T) {
	home := showFixture(t)
	code, out, errOut := capture(t, "config", "show", "--defaults")
	if code != 0 {
		t.Fatalf("config show --defaults = %d, stderr %q", code, errOut)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("output to a file holds an escape sequence:\n%s", out)
	}
	if strings.Contains(out, home) {
		t.Errorf("output names the home directory; want ~:\n%s", out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	find := func(key string) string {
		for _, l := range lines {
			if strings.HasPrefix(l[2:], key+" = ") {
				return l
			}
		}
		t.Errorf("no line for %s in\n%s", key, out)
		return ""
	}
	for _, c := range []struct{ key, prefix, suffix string }{
		{"learn.map_workers", "* learn.map_workers = 2 ", "  ~/config.d/50-x.local.toml:1"},
		{"harness.kiro.enabled", "* harness.kiro.enabled = false ", "  ~/config.d/50-x.local.toml:3"},
		{"store.kind", `* store.kind = "fs" `, "  ~/config.toml:3"},
		{"store.path", `  store.path = "~/recall-store"`, `"~/recall-store"`},
		{"harness.<name>.enabled", "  harness.<name>.enabled = true", "true"},
		{"learn.check", "  learn.check = (unset)", "(unset)"},
	} {
		l := find(c.key)
		if !strings.HasPrefix(l, c.prefix) || !strings.HasSuffix(l, c.suffix) {
			t.Errorf("line %q, want prefix %q and suffix %q", l, c.prefix, c.suffix)
		}
	}
	// Each set instance follows its pattern's line.
	var pat, inst int
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "  harness.<name>.enabled "):
			pat = i
		case strings.HasPrefix(l, "* harness.kiro.enabled "):
			inst = i
		}
	}
	if inst != pat+1 {
		t.Errorf("harness.kiro.enabled at line %d, want right after harness.<name>.enabled at %d", inst, pat)
	}
	if n := strings.Count(out, "\n* "); n != 4 {
		t.Errorf("%d marked lines, want the 4 keys the files set:\n%s", n, out)
	}
}

func TestConfigShowDefaultsSkipsUnknownKey(t *testing.T) {
	unknownKeyHome(t, "")
	code, out, errOut := capture(t, "config", "show", "--defaults")
	if code != 0 || strings.Contains(out, "knid") {
		t.Errorf("config show --defaults = %d, stdout lists the unknown key:\n%s", code, out)
	}
	if !strings.Contains(errOut, `"store.knid"`) {
		t.Errorf("stderr %q lacks the warning", errOut)
	}
}

func TestConfigShowRejectsUnknownFlag(t *testing.T) {
	showFixture(t)
	if code, _, _ := capture(t, "config", "show", "--default"); code != 2 {
		t.Errorf("config show --default = %d, want 2", code)
	}
}

func TestShowDefaultsBold(t *testing.T) {
	home := showFixture(t)
	c, err := loadConfig(config.DefaultBase(home))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := showDefaults(&b, c, home, "laptop-a", true); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "\x1b[1mlearn.map_workers = 2 ") || strings.Contains(out, "\n* ") {
		t.Errorf("terminal output doesn't bold the set keys:\n%s", out)
	}
}

func TestConfigShowDefaultsShortHost(t *testing.T) {
	tempHome(t, "")
	resetConfigWarnings()
	old := osHostname
	osHostname = func() (string, error) { return "laptop-a.example.com", nil }
	t.Cleanup(func() { osHostname = old })
	_, out, _ := capture(t, "config", "show", "--defaults")
	if !strings.Contains(out, "  host = \"laptop-a\"\n") {
		t.Errorf("want host = \"laptop-a\" in\n%s", out)
	}
}

func TestConfigShowHomeTrailingSlash(t *testing.T) {
	home := showFixture(t)
	t.Setenv("HOME", home+"/")
	for _, args := range [][]string{{"config", "show"}, {"config", "show", "--defaults"}} {
		_, out, _ := capture(t, args...)
		if strings.Contains(out, home) || !strings.Contains(out, "~/config.toml:1") {
			t.Errorf("%v names the home directory, want ~:\n%s", args, out)
		}
	}
}

func TestConfigShowDefaultsKnownSources(t *testing.T) {
	showFixture(t)
	_, out, _ := capture(t, "config", "show", "--defaults")
	want := "  harness.<name>.source = (unset)\n  harness.claude.source = \"~/.claude/projects\"\n  harness.kiro.source = \"~/.kiro/sessions\"\n"
	if !strings.Contains(out, want) {
		t.Errorf("want\n%s\nin\n%s", want, out)
	}
}

func TestConfigShowDefaultsSetKnownSource(t *testing.T) {
	resetConfigWarnings()
	tempHome(t, "[harness.kiro]\nsource = \"~/k\"\n")
	_, out, _ := capture(t, "config", "show", "--defaults")
	if !strings.Contains(out, "* harness.kiro.source = \"~/k\"") || strings.Count(out, "harness.kiro.source") != 1 {
		t.Errorf("want one marked harness.kiro.source line in\n%s", out)
	}
}

func TestConfigShowDefaultsKiroOnly(t *testing.T) {
	resetConfigWarnings()
	tempHome(t, "[harness.claude]\nenabled = false\n[harness.kiro]\n")
	_, out, _ := capture(t, "config", "show", "--defaults")
	for _, want := range []string{`  learn.runner = "kiro"`, `  learn.targets = ["~/.kiro/steering/AGENTS.md"]`, `  learn.map_model = "auto"`} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("want %s in\n%s", want, out)
		}
	}
}
