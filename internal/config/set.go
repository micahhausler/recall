package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// SetFile is the one drop-in `recall config set` writes. It sorts late in
// config.d, so its values win, and `config show` makes that visible. The
// -local suffix is the convention for machine-local drop-ins, so a dotfiles
// repo can ignore config.d/*.local.toml by pattern and sync the rest.
const SetFile = "90-set.local.toml"

const setHeader = `# Managed by recall: written by 'recall config set'.
# Values here win over earlier config files. Edit with 'recall config set',
# or move a value you want to own into your own config file.
`

// Set writes one dotted key to the tool-owned drop-in next to base, leaving
// every other file untouched. The raw value is decoded as a TOML value
// (so true, 42, 0.5, and ["a"] keep their types); anything that doesn't
// parse as one is a string.
func Set(base, key, raw string) (string, error) {
	if key == "" || strings.HasPrefix(key, ".") || strings.HasSuffix(key, ".") || strings.Contains(key, "..") {
		return "", fmt.Errorf("bad key %q", key)
	}
	for _, part := range strings.Split(key, ".") {
		if part == "" || strings.ContainsAny(part, " \t=#[]\"'") {
			return "", fmt.Errorf("bad key %q", key)
		}
	}
	target := filepath.Join(filepath.Dir(base), "config.d", SetFile)
	tree := map[string]any{}
	if data, err := os.ReadFile(target); err == nil {
		if err := toml.Unmarshal(data, &tree); err != nil {
			return "", fmt.Errorf("%s: %w (fix or remove it; recall owns this file)", target, err)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}

	setLeaf(tree, strings.Split(key, "."), parseValue(raw))

	// Validate the would-be merged config before writing anything.
	if err := validateTree(tree, target); err != nil {
		return "", err
	}
	out, err := toml.Marshal(tree)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(setHeader), out...), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return target, nil
}

func setLeaf(tree map[string]any, path []string, value any) {
	if len(path) == 1 {
		tree[path[0]] = value
		return
	}
	sub, ok := tree[path[0]].(map[string]any)
	if !ok {
		sub = map[string]any{}
		tree[path[0]] = sub
	}
	setLeaf(sub, path[1:], value)
}

// parseValue decodes raw as a TOML value; a raw that isn't one is a string.
func parseValue(raw string) any {
	var doc map[string]any
	if err := toml.Unmarshal([]byte("v = "+raw+"\n"), &doc); err == nil {
		if v, ok := doc["v"]; ok {
			if _, isTable := v.(map[string]any); !isTable {
				return v
			}
		}
	}
	return raw
}

// validateTree checks a single file's tree against the schema.
func validateTree(tree map[string]any, file string) error {
	origins := map[string]Origin{}
	mergeInto(map[string]any{}, tree, origins, file, nil, "")
	c := &Config{values: tree, origin: origins}
	return c.validate()
}
