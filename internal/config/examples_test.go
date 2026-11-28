package config

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

const (
	referencePath = "../../config.reference.toml"
	starterPath   = "../../config.example.toml"
)

// undocumented: schema keys the reference leaves out, each named for the change that deletes it.
const noHook = "the change that removes the session-end hook"

var undocumented = map[string]string{
	"hooks.map":         noHook,
	"hooks.archive":     noHook,
	"hooks.learn_every": noHook,
}

// commentedKey matches a commented-out "# key = value" line.
var commentedKey = regexp.MustCompile(`^# ([a-z][a-z0-9_-]*(?:\.[a-z0-9_-]+)*) = (.+)$`)

// looseKey matches any comment line that looks like a key.
var looseKey = regexp.MustCompile(`^\s*#\s*[a-z][\w.-]*\s*=`)

// exampleKeys returns the commented keys in order, and the file with those lines uncommented.
func exampleKeys(t *testing.T, path string) (keys []string, uncommented string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b strings.Builder
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if m := commentedKey.FindStringSubmatch(line); m != nil {
			keys = append(keys, m[1])
			line = m[1] + " = " + m[2]
		} else if looseKey.MatchString(line) {
			t.Errorf("%s: %q looks like a key but isn't written as \"# key = value\"", path, line)
		}
		b.WriteString(line + "\n")
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return keys, b.String()
}

func loadText(t *testing.T, text string) *Config {
	t.Helper()
	base := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(base, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(base)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, w := range c.Warnings() {
		t.Errorf("load: %s", w)
	}
	return c
}

func loadFile(t *testing.T, path string) *Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return loadText(t, string(data))
}

func inSchema(key string) bool {
	for _, p := range schema {
		if matches(p, key) {
			return true
		}
	}
	return false
}

// referenceKeys is every key the reference names: its live keys and its commented ones.
func referenceKeys(t *testing.T) []string {
	t.Helper()
	keys, uncommented := exampleKeys(t, referencePath)
	// The uncommented file must load too, so every commented line is valid TOML in the schema.
	all := loadText(t, uncommented).Leaves()
	if len(all) != len(keys) {
		t.Fatalf("reference names %d keys, but loads %d with every key uncommented", len(keys), len(all))
	}
	return keys
}

func TestReferenceSetsNothing(t *testing.T) {
	if live := loadFile(t, referencePath).Leaves(); len(live) != 0 {
		t.Errorf("config.reference.toml sets %v; comment every key out", live)
	}
}

// TestReferenceTimerIsWhatInitWrites holds the timer decision to the jobs Fresh plans without a session-end hook.
func TestReferenceTimerIsWhatInitWrites(t *testing.T) {
	_, uncommented := exampleKeys(t, referencePath)
	ref := loadText(t, uncommented)
	jobs := freshKeys(t, "[harness.claude]\nenabled = false\n[harness.kiro]\n", "/usr/bin:/bin")
	n := 0
	for k, raw := range jobs {
		if !strings.HasPrefix(k, "recur.jobs.") {
			continue
		}
		n++
		got, ok := ref.Get(k)
		if !ok {
			t.Errorf("config.reference.toml lacks %q, which recall init writes for a timer", k)
			continue
		}
		var want map[string]any
		if err := toml.Unmarshal([]byte("v = "+raw), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want["v"]) {
			t.Errorf("config.reference.toml shows %s = %v; recall init writes %s", k, got, raw)
		}
	}
	if n == 0 {
		t.Fatal("Fresh planned no recur jobs")
	}
}

func TestReferenceKeysInSchema(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range referenceKeys(t) {
		if seen[k] {
			t.Errorf("config.reference.toml names %q twice", k)
		}
		seen[k] = true
		if !inSchema(k) {
			t.Errorf("config.reference.toml names %q, which no schema pattern matches", k)
		}
		if pr, ok := undocumented[k]; ok {
			t.Errorf("config.reference.toml names %q, which %s deletes; leave it out", k, pr)
		}
	}
}

func TestReferenceCoversSchema(t *testing.T) {
	keys := referenceKeys(t)
	for _, p := range schema {
		if _, ok := undocumented[p]; ok {
			continue
		}
		found := false
		for _, k := range keys {
			if matches(p, k) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("config.reference.toml has no key for schema pattern %q", p)
		}
	}
	for k, pr := range undocumented {
		if !inSchema(k) {
			t.Errorf("%q left the schema in %s; drop it from undocumented", k, pr)
		}
	}
}

// TestReferenceSettings checks every sample value, except repo_notes, which needs a git checkout.
func TestReferenceSettings(t *testing.T) {
	_, uncommented := exampleKeys(t, referencePath)
	var kept []string
	for _, line := range strings.Split(uncommented, "\n") {
		if !strings.HasPrefix(line, "learn.repo_notes = ") {
			kept = append(kept, line)
		}
	}
	if _, err := loadText(t, strings.Join(kept, "\n")).Settings(t.TempDir(), "laptop-a"); err != nil {
		t.Fatal(err)
	}
}

// freshKeys is what Fresh plans on an empty config.
func freshKeys(t *testing.T, text, path string) map[string]string {
	t.Helper()
	plan, _ := Fresh(loadText(t, text), false, "laptop-a", nil, path)
	out := map[string]string{}
	for _, kv := range plan {
		out[kv.Key] = kv.Value
	}
	return out
}

func TestStarterIsWhatInitWrites(t *testing.T) {
	c := loadFile(t, starterPath)
	if _, err := c.Settings(t.TempDir(), "laptop-a"); err != nil {
		t.Fatal(err)
	}
	// The only commented keys are the runner and its model defaults, at their defaults.
	commented, uncommented := exampleKeys(t, starterPath)
	claudeDefaults, _ := defaultsFor(HarnessClaude)
	models := map[string]string{
		"learn.runner":       HarnessClaude,
		"learn.map_model":    claudeDefaults.mapModel,
		"learn.reduce_model": claudeDefaults.reduceModel,
		"learn.apply_model":  claudeDefaults.applyModel,
	}
	shown := loadText(t, uncommented)
	for _, k := range commented {
		want, ok := models[k]
		if !ok {
			t.Errorf("config.example.toml comments out %q; the starter sets each key it names but the runner and its models", k)
			continue
		}
		if got, _ := shown.Get(k); got != want {
			t.Errorf("config.example.toml shows %s = %v; the claude runner's default is %q", k, got, want)
		}
		delete(models, k)
	}
	for k := range models {
		t.Errorf("config.example.toml lacks %q, commented out at its default", k)
	}
	claude := freshKeys(t, "", "")
	kiro := freshKeys(t, "[harness.claude]\nenabled = false\n[harness.kiro]\n", "/usr/bin:/bin")
	starter := c.Leaves()
	for _, k := range starter {
		if !inSchema(k) {
			t.Errorf("config.example.toml sets %q, which no schema pattern matches", k)
		}
		_, onClaude := claude[k]
		_, onKiro := kiro[k]
		if !onClaude && !onKiro {
			t.Errorf("config.example.toml sets %q, which recall init never writes", k)
		}
	}
	for k, raw := range claude {
		got, ok := c.Get(k)
		if !ok {
			t.Errorf("config.example.toml lacks %q, which recall init writes on a Claude Code machine", k)
			continue
		}
		var want map[string]any
		if err := toml.Unmarshal([]byte("v = "+raw), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want["v"]) {
			t.Errorf("config.example.toml sets %s = %v; recall init writes %s", k, got, raw)
		}
	}
}
