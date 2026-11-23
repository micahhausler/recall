package recall

import (
	"io/fs"
	"path"
	"strings"
	"testing"
)

// Kiro rejects a skill whose frontmatter isn't strict YAML, and an
// unquoted value holding ": " is the usual way to break it.
func TestKiroPowerFrontmatter(t *testing.T) {
	power := KiroPower()
	skills, err := fs.Glob(power, "skills/*/SKILL.md")
	if err != nil || len(skills) == 0 {
		t.Fatalf("skills = %v, %v", skills, err)
	}
	for _, p := range skills {
		data, err := fs.ReadFile(power, p)
		if err != nil {
			t.Fatal(err)
		}
		front, ok := strings.CutPrefix(string(data), "---\n")
		if !ok {
			t.Errorf("%s: no frontmatter", p)
			continue
		}
		front, _, ok = strings.Cut(front, "\n---\n")
		if !ok {
			t.Errorf("%s: frontmatter never closes", p)
			continue
		}
		keys := map[string]string{}
		for _, line := range strings.Split(front, "\n") {
			key, value, ok := strings.Cut(line, ": ")
			if !ok {
				t.Errorf("%s: %q isn't a key: value line", p, line)
				continue
			}
			quoted := len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"'
			if !quoted && strings.Contains(value, ": ") {
				t.Errorf("%s: %s holds \": \" unquoted, which strict YAML rejects", p, key)
			}
			keys[key] = value
		}
		if dir := path.Base(path.Dir(p)); keys["name"] != dir {
			t.Errorf("%s: name %q, want its folder %q", p, keys["name"], dir)
		}
		if keys["description"] == "" {
			t.Errorf("%s: no description", p)
		}
	}
}
