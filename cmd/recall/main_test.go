package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/config"
)

// Who calls each command: true is a person, in `recall --help`; false is
// the timer, a skill, a hook, or a script, in `recall --help --all` only.
var commandAudience = map[string]bool{
	"init":          true,
	"report":        true,
	"proposals":     true,
	"config":        true,
	"observe":       false,
	"archive":       false,
	"propose":       false,
	"import":        false,
	"lint":          false,
	"skills":        false,
	"hook":          false,
	"extension":     false,
	"events":        false,
	"recur":         false,
	"learn":         false,
	"archive-chats": false,
}

// capture runs recall with args and returns its exit code, stdout, and
// stderr.
func capture(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	out, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	errf, err := os.Create(filepath.Join(dir, "err"))
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, errf
	code := run(args)
	os.Stdout, os.Stderr = stdout, stderr
	out.Close()
	errf.Close()
	o, _ := os.ReadFile(out.Name())
	e, _ := os.ReadFile(errf.Name())
	return code, string(o), string(e)
}

// tempHome points HOME and the config at a temp dir.
func tempHome(t *testing.T, toml string) string {
	t.Helper()
	home := t.TempDir()
	cfg := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfg, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(config.EnvVar, cfg)
	return home
}

func TestCommandTableMatchesAudience(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range commands {
		seen[c.name] = true
		visible, ok := commandAudience[c.name]
		if !ok {
			t.Errorf("recall %s dispatches but has no row in commandAudience", c.name)
			continue
		}
		if c.visible != visible {
			t.Errorf("recall %s: visible = %v, want %v", c.name, c.visible, visible)
		}
	}
	for name := range commandAudience {
		if !seen[name] {
			t.Errorf("recall %s has a row but doesn't dispatch", name)
		}
	}
}

func helpLines(text string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == "recall" && strings.HasPrefix(line, "  ") {
			names[f[1]] = true
		}
	}
	return names
}

func TestHelpListsByAudience(t *testing.T) {
	code, plain, _ := capture(t, "--help")
	if code != 0 {
		t.Fatalf("--help = %d", code)
	}
	code, all, _ := capture(t, "--help", "--all")
	if code != 0 {
		t.Fatalf("--help --all = %d", code)
	}
	plainNames, allNames := helpLines(plain), helpLines(all)
	for name, visible := range commandAudience {
		if plainNames[name] != visible {
			t.Errorf("recall %s in plain help = %v, want %v", name, plainNames[name], visible)
		}
		if !allNames[name] {
			t.Errorf("recall %s missing from --help --all", name)
		}
	}
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "recall --help --all") {
		t.Errorf("plain help ends %q, want the --help --all line", last)
	}
	if strings.Contains(all, last) {
		t.Errorf("--help --all carries plain help's closing line %q", last)
	}
	for _, args := range [][]string{{"help", "--all"}, {"-h", "--all"}} {
		if code, out, _ := capture(t, args...); code != 0 || out != all {
			t.Errorf("recall %v = %d, %q; want the --help --all text", args, code, out)
		}
	}
	for _, args := range [][]string{{"help"}, {"-h"}} {
		if code, out, _ := capture(t, args...); code != 0 || out != plain {
			t.Errorf("recall %v = %d; want the --help text", args, code)
		}
	}
	if code, _, errText := capture(t, "--all"); code != 2 || !strings.Contains(errText, "unknown command") {
		t.Errorf("recall --all = %d, %q; want an unknown command", code, errText)
	}
}

// Every command, hidden or not, dispatches and prints its own usage.
func TestEveryCommandPrintsUsage(t *testing.T) {
	tempHome(t, "")
	for name := range commandAudience {
		_, out, errText := capture(t, name, "-h")
		text := out + errText
		if strings.Contains(text, "unknown command") || !strings.Contains(text, "Usage:") {
			t.Errorf("recall %s -h printed %q", name, text)
		}
	}
	_, _, errText := capture(t, "archive", "build", "-h")
	if !strings.Contains(errText, "Usage:") {
		t.Errorf("recall archive build -h printed %q", errText)
	}
}

type learnCall struct {
	verb string
	o    learnOpts
}

type archiveCall struct {
	verb string
	o    archiveOpts
	args []string
}

func recordExec(t *testing.T) (*[]learnCall, *[]archiveCall) {
	t.Helper()
	var lc []learnCall
	var ac []archiveCall
	oldL, oldA := learnExec, archiveExec
	t.Cleanup(func() { learnExec, archiveExec = oldL, oldA })
	learnExec = func(verb string, o learnOpts) int {
		lc = append(lc, learnCall{verb, o})
		return 0
	}
	archiveExec = func(verb string, o archiveOpts, args []string) int {
		ac = append(ac, archiveCall{verb, o, append([]string{}, args...)})
		return 0
	}
	return &lc, &ac
}

// Each new spelling reaches the same step with the same flags as its old
// one.
func TestNewSpellingsMatchOld(t *testing.T) {
	for _, tc := range []struct{ new, old []string }{
		{[]string{"observe", "--since", "all", "--limit", "3", "--idle", "6h", "--archives", "--dry-run"},
			[]string{"learn", "map", "--since", "all", "--limit", "3", "--idle", "6h", "--archives", "--dry-run"}},
		{[]string{"observe"}, []string{"learn", "map"}},
		{[]string{"propose", "--out", "/tmp/out", "--force"}, []string{"learn", "run", "--out", "/tmp/out", "--force"}},
		{[]string{"propose", "--window", "365d"}, []string{"learn", "run", "--window", "365d"}},
		{[]string{"proposals"}, []string{"learn", "reviews"}},
		{[]string{"report", "--since", "7d"}, []string{"learn", "report", "--since", "7d"}},
		{[]string{"report"}, []string{"learn", "report"}},
		{[]string{"archive", "--lookback", "2", "--dry-run", "--offline"}, []string{"archive-chats", "run", "--lookback", "2", "--dry-run", "--offline"}},
		{[]string{"archive"}, []string{"archive-chats"}},
		{[]string{"archive"}, []string{"archive-chats", "run"}},
		{[]string{"archive", "build", "2026-W45", "out.tar.zst"}, []string{"archive-chats", "build", "2026-W45", "out.tar.zst"}},
	} {
		lc, ac := recordExec(t)
		newCode, _, newErr := capture(t, tc.new...)
		oldCode, _, oldErr := capture(t, tc.old...)
		if newCode != 0 || oldCode != 0 {
			t.Errorf("%v = %d (%s), %v = %d (%s)", tc.new, newCode, newErr, tc.old, oldCode, oldErr)
			continue
		}
		switch {
		case len(*lc) == 2 && len(*ac) == 0:
			if !reflect.DeepEqual((*lc)[0], (*lc)[1]) {
				t.Errorf("%v reached %+v, %v reached %+v", tc.new, (*lc)[0], tc.old, (*lc)[1])
			}
		case len(*ac) == 2 && len(*lc) == 0:
			if !reflect.DeepEqual((*ac)[0], (*ac)[1]) {
				t.Errorf("%v reached %+v, %v reached %+v", tc.new, (*ac)[0], tc.old, (*ac)[1])
			}
		default:
			t.Errorf("%v and %v reached learn %v, archive %v", tc.new, tc.old, *lc, *ac)
		}
	}
	lc, _ := recordExec(t)
	capture(t, "observe")
	if got := (*lc)[0].o; got.since != "30d" || got.idle != 0 {
		t.Errorf("observe defaults = %+v", got)
	}
}

// A new command takes only its own flags and words, and never runs on a
// mistyped one.
func TestNewSpellingsRejectOthersFlags(t *testing.T) {
	for _, args := range [][]string{
		{"observe", "--take"},
		{"observe", "--out", "/tmp/out"},
		{"propose", "--since", "7d"},
		{"propose", "--dry-run"},
		{"proposals", "--since", "7d"},
		{"report", "--limit", "3"},
		{"archive", "--setup"},
		{"archive", "check"},
		{"archive", "upload"},
		{"archive", "--dry-run", "extra"},
		{"archive", "build", "--dry-run", "2026-W45", "out.tar.zst"},
		{"observe", "extra"},
		{"learn", "run", "--dry-run"},
	} {
		lc, ac := recordExec(t)
		code, _, _ := capture(t, args...)
		if code != 2 || len(*lc)+len(*ac) != 0 {
			t.Errorf("recall %v = %d, ran %v %v; want 2 and nothing run", args, code, *lc, *ac)
		}
	}
	code, _, errText := capture(t, "propose", "--dry-run")
	if code != 2 || !strings.Contains(errText, "learn reduce --dry-run") {
		t.Errorf("propose --dry-run = %d, %q", code, errText)
	}
}

// The new spellings run the real step: with nothing sent for review yet,
// both list the same empty set.
func TestProposalsRunsReviews(t *testing.T) {
	tempHome(t, "host = \"laptop-a\"\n[learn]\nreview = \"branch\"\n")
	newCode, newOut, newErr := capture(t, "proposals")
	oldCode, oldOut, _ := capture(t, "learn", "reviews")
	if newCode != 0 || oldCode != 0 || newOut != oldOut || !strings.Contains(newOut, "recall propose") {
		t.Errorf("proposals = %d %q %q; learn reviews = %d %q", newCode, newOut, newErr, oldCode, oldOut)
	}
}

// Every flag an old spelling took at the rename still parses on every one
// of its verbs.
func TestOldSpellingsKeepEveryFlag(t *testing.T) {
	learnFlagArgs := []string{"--since", "7d", "--limit", "3", "--archives", "--idle", "6h", "--out", "/tmp/out",
		"--window", "all", "--from", "/tmp/from", "--dry-run", "--force", "--take", "--harness", "claude"}
	for _, verb := range []string{"map", "observations", "candidates", "report", "rules", "targets", "age", "reduce", "apply", "reviews", "reducer", "run"} {
		args := append([]string{"learn", verb}, learnFlagArgs...)
		if verb == "rules" {
			args = append([]string{"learn", "rules", "sync"}, learnFlagArgs...)
		}
		if verb == "run" {
			args = removeArg(args, "--dry-run")
		}
		lc, _ := recordExec(t)
		if code, _, errText := capture(t, args...); code != 0 || len(*lc) != 1 {
			t.Errorf("recall %v = %d, %q; want it parsed and run", args, code, errText)
		}
	}
	archiveFlagArgs := []string{"--lookback", "2", "--dry-run", "--offline", "--setup"}
	for _, verb := range []string{"run", "build", "info", "estimate", "check", "policy", "setup"} {
		args := append([]string{"archive-chats", verb}, archiveFlagArgs...)
		_, ac := recordExec(t)
		if code, _, errText := capture(t, args...); code != 0 || len(*ac) != 1 {
			t.Errorf("recall %v = %d, %q; want it parsed and run", args, code, errText)
		}
	}
}

func removeArg(args []string, drop string) []string {
	var out []string
	for _, a := range args {
		if a != drop {
			out = append(out, a)
		}
	}
	return out
}

// Observe's own output says observe.
func TestObserveOutputSaysObserve(t *testing.T) {
	tempHome(t, "host = \"laptop-a\"\n[learn]\nreview = \"branch\"\n")
	for _, args := range [][]string{{"observe", "--dry-run"}, {"observe"}} {
		code, out, errText := capture(t, args...)
		if code != 0 || !strings.Contains(out, args[0]+":") && !strings.Contains(out, "observe --dry-run:") {
			t.Errorf("recall %v = %d, %q, %q", args, code, out, errText)
		}
		if strings.Contains(out, "map") {
			t.Errorf("recall %v printed map: %q", args, out)
		}
	}
}

// Build with the wrong arguments prints the usage of the spelling called.
func TestArchiveBuildUsage(t *testing.T) {
	tempHome(t, "host = \"laptop-a\"\n")
	if code, _, errText := capture(t, "archive", "build", "2026-W45"); code != 2 || !strings.Contains(errText, "recall archive build") || strings.Contains(errText, "recall archive-chats build") {
		t.Errorf("archive build with one argument = %d, %q", code, errText)
	}
	if code, _, errText := capture(t, "archive-chats", "build", "2026-W45"); code != 2 || !strings.Contains(errText, "recall archive-chats build") {
		t.Errorf("archive-chats build with one argument = %d, %q", code, errText)
	}
}

// The scheduler and binary path are Go parameters now, not flags.
func TestRecurTestFlagsGone(t *testing.T) {
	tempHome(t, "")
	for _, args := range [][]string{{"recur", "--scheduler", "cron", "check"}, {"recur", "--self", "/x/recall", "check"}} {
		code, _, errText := capture(t, args...)
		if code != 2 || !strings.Contains(errText, "flag provided but not defined") {
			t.Errorf("recall %v = %d, %q; want an unknown flag", args, code, errText)
		}
	}
}
