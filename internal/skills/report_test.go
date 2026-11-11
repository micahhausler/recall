package skills

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/recall/internal/events"
)

func writeSkill(t *testing.T, dir, folder, frontName string) {
	t.Helper()
	path := filepath.Join(dir, folder, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# no frontmatter\n"
	if frontName != "" {
		body = "---\nname: " + frontName + "\ndescription: d\n---\nbody\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscover(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "alpha")
	writeSkill(t, dir, "beta-dir", "beta")
	writeSkill(t, dir, "gamma", "") // no frontmatter: directory name wins
	extra := t.TempDir()
	writeSkill(t, extra, "alpha", "alpha") // duplicate name: first dir wins
	writeSkill(t, extra, "delta", "delta")
	got, err := Discover([]string{dir, extra})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(got))
	for i, s := range got {
		names[i] = s.Name
	}
	want := []string{"alpha", "beta", "gamma", "delta"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v", names, want)
	}
	if !strings.HasPrefix(got[0].Path, dir) {
		t.Errorf("alpha path = %s, want the first dir's copy", got[0].Path)
	}
}

func ev(kind events.Kind, skill, session, ts string) events.Event {
	return events.Event{Harness: "claude", Session: session, Timestamp: ts, Kind: kind, Skill: skill}
}

func TestReportCountsAndFlags(t *testing.T) {
	found := []Skill{
		{Name: "used-both", Path: "/s/used-both/SKILL.md"},
		{Name: "slash-only", Path: "/s/slash-only/SKILL.md"},
		{Name: "unused", Path: "/s/unused/SKILL.md"},
	}
	evs := []events.Event{
		ev(events.SkillInvoked, "used-both", "s1", "2026-11-01T10:00:00Z"),
		ev(events.SkillInvoked, "used-both", "s2", "2026-11-03T10:00:00Z"),
		ev(events.SlashCommand, "used-both", "s1", "2026-11-02T10:00:00Z"),
		ev(events.SlashCommand, "slash-only", "s1", "2026-11-01T10:00:00Z"),
		ev(events.SlashCommand, "slash-only", "s3", "2026-11-04T10:00:00Z"),
		ev(events.SkillInvoked, "ghost", "s1", "2026-11-01T10:00:00Z"),
		ev(events.ToolCall, "", "s1", "2026-11-01T10:00:00Z"), // not a skill event
	}
	rows := Report(found, nil, evs)
	byName := map[string]Row{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	ub := byName["used-both"]
	if ub.ByModel != 2 || ub.BySlash != 1 || ub.Sessions != 2 || ub.LastUsed != "2026-11-03T10:00:00Z" || ub.Flag != "" {
		t.Errorf("used-both = %+v", ub)
	}
	if so := byName["slash-only"]; so.Flag != FlagSlashOnly || so.BySlash != 2 {
		t.Errorf("slash-only = %+v", so)
	}
	if un := byName["unused"]; un.Flag != FlagRetire || un.ByModel+un.BySlash != 0 {
		t.Errorf("unused = %+v", un)
	}
	if g := byName["ghost"]; g.Flag != FlagNotOnDisk || g.ByModel != 1 {
		t.Errorf("ghost = %+v", g)
	}
	// Most-used first.
	if rows[0].Name != "used-both" {
		t.Errorf("order = %v", rows)
	}
}

func TestReportBuiltinsAndSkillDirs(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "project-skill", "project-skill")
	found := []Skill{{Name: "listed"}} // from the harness inventory: no path
	commands := map[string]bool{"compact": true, "listed": true}
	evs := []events.Event{
		{Kind: events.BuiltinCommand, Skill: "clear", Session: "s1"},
		{Kind: events.SlashCommand, Skill: "compact", Session: "s1"},
		{Kind: events.SlashCommand, Skill: "listed", Session: "s1"},
		{Kind: events.SlashCommand, Skill: "project-skill", SkillDir: filepath.Join(dir, "project-skill"), Session: "s1"},
		{Kind: events.SlashCommand, Skill: "gone", SkillDir: filepath.Join(dir, "gone"), Session: "s1"},
	}
	byName := map[string]Row{}
	for _, r := range Report(found, commands, evs) {
		byName[r.Name] = r
	}
	for _, name := range []string{"clear", "compact"} {
		if _, ok := byName[name]; ok {
			t.Errorf("%s is a built-in, not a skill", name)
		}
	}
	if r := byName["listed"]; r.Flag != FlagSlashOnly || r.BySlash != 1 {
		t.Errorf("listed = %+v", r)
	}
	if r := byName["project-skill"]; r.Flag == FlagNotOnDisk || r.Path == "" {
		t.Errorf("project-skill = %+v, want found via its recorded dir", r)
	}
	if r := byName["gone"]; r.Flag != FlagNotOnDisk {
		t.Errorf("gone = %+v, want not on disk", r)
	}
}

func TestPrintAlignsLongNames(t *testing.T) {
	var buf bytes.Buffer
	Print(&buf, []Row{
		{Name: "short", ByModel: 1},
		{Name: "a-plugin-with-a-long-name:some-skill", BySlash: 2},
	}, time.Time{})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	col := strings.Index(lines[0], "MODEL") + len("MODEL")
	for _, l := range lines[1:] {
		if len(l) < col || l[col-1] == ' ' {
			t.Errorf("MODEL column misaligned in %q (header %q)", l, lines[0])
		}
	}
}
