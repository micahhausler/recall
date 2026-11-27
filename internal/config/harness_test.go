package config

import (
	"slices"
	"strings"
	"testing"
)

// Every known harness, on by itself, gives the loop everything it needs
// with no other key set: a new entry in the harness table can't leave a
// default out.
func TestKnownHarnessDefaults(t *testing.T) {
	const home = "/home/you"
	for _, name := range KnownHarnesses {
		var toml strings.Builder
		for _, other := range KnownHarnesses {
			toml.WriteString("[harness." + other + "]\n")
			if other != name {
				toml.WriteString("enabled = false\n")
			}
		}
		b := base(t)
		write(t, b, toml.String())
		c, err := Load(b)
		if err != nil {
			t.Fatal(err)
		}
		s, err := c.Settings(home, "laptop-a")
		if err != nil {
			t.Fatal(err)
		}
		l := s.Learn
		if l.Runner != name || len(l.Targets) != 1 || l.Targets[0] != DefaultTarget(name, home) {
			t.Errorf("%s: runner %q, targets %v", name, l.Runner, l.Targets)
		}
		for what, got := range map[string]string{
			"cli": DefaultCLI(name), "source": s.Harnesses[name].Source, "target": DefaultTarget(name, home),
			"skills": l.Skills, "trailer": l.EvidenceTrailer,
			"map model": l.MapModel, "reduce model": l.ReduceModel, "apply model": l.ApplyModel,
		} {
			if got == "" {
				t.Errorf("%s: no default %s", name, what)
			}
		}
	}
}

// A harness recall doesn't know has no defaults, rather than another's.
func TestUnknownHarnessHasNoDefaults(t *testing.T) {
	for what, got := range map[string]string{
		"cli": DefaultCLI("other"), "source": DefaultSource("other", "/home/you"), "target": DefaultTarget("other", "/home/you"),
		"skills": DefaultSkills("other", "/home/you"), "trailer": DefaultTrailer("other"),
	} {
		if got != "" {
			t.Errorf("unknown harness: default %s = %q, want none", what, got)
		}
	}
}

func TestHookRunsLoop(t *testing.T) {
	on := HarnessSettings{Enabled: true}
	for _, tc := range []struct {
		enabled map[string]HarnessSettings
		want    bool
	}{
		{map[string]HarnessSettings{HarnessClaude: on}, true},
		{map[string]HarnessSettings{HarnessClaude: on, HarnessKiro: on}, true},
		{map[string]HarnessSettings{HarnessKiro: on}, false},
		{map[string]HarnessSettings{"other": on}, false},
		{nil, false},
	} {
		if got := HookRunsLoop(tc.enabled); got != tc.want {
			t.Errorf("HookRunsLoop(%v) = %v, want %v", tc.enabled, got, tc.want)
		}
	}
}

func TestRepoNoteHarnesses(t *testing.T) {
	on := HarnessSettings{Enabled: true}
	for _, tc := range []struct {
		enabled map[string]HarnessSettings
		want    []string
	}{
		{map[string]HarnessSettings{HarnessClaude: on, HarnessKiro: on}, []string{HarnessClaude}},
		{map[string]HarnessSettings{HarnessKiro: on}, nil},
		{nil, nil},
	} {
		if got := RepoNoteHarnesses(tc.enabled); !slices.Equal(got, tc.want) {
			t.Errorf("RepoNoteHarnesses(%v) = %v, want %v", tc.enabled, got, tc.want)
		}
	}
}
