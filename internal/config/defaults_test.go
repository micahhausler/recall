package config

import (
	"reflect"
	"strings"
	"testing"
)

// noDefault: keys the code gives no default on an empty config.
var noDefault = map[string]bool{
	"store.s3.profile":              true,
	"store.s3.region":               true,
	"store.s3.bucket":               true,
	"store.s3.endpoint":             true,
	"harness.<name>.source":         true,
	"harness.<name>.format":         true,
	"harness.<name>.import.command": true,
	"recur.env":                     true,
	"recur.cron_tz":                 true,
	"recur.jobs.<name>.schedule":    true,
	"recur.jobs.<name>.command":     true,
	"learn.map_effort":              true,
	"learn.reduce_effort":           true,
	"learn.apply_effort":            true,
	"learn.repos":                   true,
	"learn.repo_notes":              true,
	"learn.settings":                true,
	"learn.check":                   true,
	"learn.evidence":                true,
	"lint.ignore":                   true,
	"keep.public":                   true,
	"review.assignees":              true,
	"review.credentials.command":    true,
	"extensions.<name>":             true,
}

func defaultsOf(t *testing.T, c *Config, home string) map[string]string {
	t.Helper()
	defs, err := Defaults(c, home, "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range defs {
		got[d.Key] = d.Value
	}
	return got
}

func TestDefaultsListEverySchemaPattern(t *testing.T) {
	defs, err := Defaults(nil, "/home/you", "laptop-a")
	if err != nil {
		t.Fatal(err)
	}
	var patterns []string
	for _, d := range defs {
		if len(patterns) == 0 || patterns[len(patterns)-1] != d.Pattern {
			patterns = append(patterns, d.Pattern)
		}
		if !d.Matches(strings.ReplaceAll(d.Key, NamePlaceholder, "x")) {
			t.Errorf("%s doesn't match its pattern %s", d.Key, d.Pattern)
		}
		if d.Value == "" {
			t.Errorf("%s has an empty value; want a default or %s", d.Key, Unset)
		}
		if strings.Contains(d.Value, "/home/you") {
			t.Errorf("%s = %s names the home directory; want ~", d.Key, d.Value)
		}
	}
	if !reflect.DeepEqual(patterns, schema) {
		t.Errorf("Defaults lists patterns\n%v\nwant the schema\n%v", patterns, schema)
	}
}

func TestDefaultsUnsetOnlyWithoutCodeDefault(t *testing.T) {
	for key, v := range defaultsOf(t, nil, "/home/you") {
		if (v == Unset) != noDefault[key] {
			t.Errorf("%s = %s; noDefault says %v", key, v, noDefault[key])
		}
	}
}

func TestDefaultsFollowSettings(t *testing.T) {
	got := defaultsOf(t, nil, "/home/you")
	for key, want := range map[string]string{
		"host":                          `"laptop-a"`,
		"store.kind":                    `"fs"`,
		"store.path":                    `"~/recall-store"`,
		"store.s3.profile":              Unset,
		"learn.map_workers":             "4",
		"learn.budget":                  "0",
		"learn.review":                  `"pr"`,
		"learn.targets":                 `["~/.claude/CLAUDE.md"]`,
		"learn.runner":                  `"claude"`,
		"learn.check":                   Unset,
		"review.credentials.timeout":    `"30s"`,
		"harness.<name>.enabled":        "true",
		"harness.<name>.import.command": Unset,
		"harness.<name>.import.timeout": `"10m"`,
		"harness.claude.source":         `"~/.claude/projects"`,
		"harness.kiro.source":           `"~/.kiro/sessions"`,
		"recur.jobs.<name>.enabled":     "true",
		"keep.bin":                      `"~/bin"`,
		"keep.targets":                  `["~/.claude/CLAUDE.md"]`,
	} {
		if got[key] != want {
			t.Errorf("%s = %s, want %s", key, got[key], want)
		}
	}
}

func TestDefaultsFollowLoadedConfig(t *testing.T) {
	for _, c := range []struct {
		name, text string
		want       map[string]string
	}{
		{"kiro only", "[harness.claude]\nenabled = false\n[harness.kiro]\n", map[string]string{
			"learn.runner":       `"kiro"`,
			"learn.targets":      `["~/.kiro/steering/AGENTS.md"]`,
			"keep.targets":       `["~/.kiro/steering/AGENTS.md"]`,
			"learn.map_model":    `"auto"`,
			"learn.reduce_model": `"auto"`,
			"learn.apply_model":  `"auto"`,
		}},
		{"two harnesses", "[harness.kiro]\n", map[string]string{
			"learn.runner":  `"claude"`,
			"learn.targets": `["~/.claude/CLAUDE.md", "~/.kiro/steering/AGENTS.md"]`,
		}},
		{"kiro runner", "learn.runner = \"kiro\"\n", map[string]string{
			"learn.map_model":    `"auto"`,
			"learn.reduce_model": `"auto"`,
		}},
	} {
		got := defaultsOf(t, loadText(t, c.text), "/home/you")
		for key, want := range c.want {
			if got[key] != want {
				t.Errorf("%s: %s = %s, want %s", c.name, key, got[key], want)
			}
		}
	}
}

func TestDefaultsHomeWithTrailingSlash(t *testing.T) {
	got := defaultsOf(t, nil, "/home/you/")
	if got["store.path"] != `"~/recall-store"` {
		t.Errorf("store.path = %s, want \"~/recall-store\"", got["store.path"])
	}
}

// TestDefaultsSchemaHasFields catches a renamed Settings field the walk would stop finding.
func TestDefaultsSchemaHasFields(t *testing.T) {
	for _, p := range schema {
		if _, ok := wildcardDefaults[p]; ok {
			continue
		}
		if _, ok := fieldType(reflect.TypeOf(Settings{}), strings.Split(p, ".")); !ok {
			t.Errorf("%s: no Settings field", p)
		}
	}
}

func TestDefaultMatches(t *testing.T) {
	d := Default{Pattern: "harness.*.source"}
	if !d.Matches("harness.kiro.source") || d.Matches("harness.kiro.enabled") || d.Matches("harness.source") {
		t.Error("Matches disagrees with the schema pattern")
	}
}
