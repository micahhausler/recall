// Package config loads recall's layered TOML config.
//
// Files: the base config (default ~/.config/recall/config.toml, overridden by
// $RECALL_CONFIG), then every config.d/*.toml next to it in lexical order.
// Files deep-merge in order: tables merge key by key, and a later scalar or
// array replaces an earlier one whole. Every merged value remembers the file
// and line it came from, so errors and `config show` can name them.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// Origin is where a merged value was set.
type Origin struct {
	File string
	Line int
}

func (o Origin) String() string {
	if o.Line == 0 {
		return o.File
	}
	return fmt.Sprintf("%s:%d", o.File, o.Line)
}

// Config is the merged view of every config file.
type Config struct {
	// Base is the base config path; config.d sits next to it.
	Base   string
	values map[string]any
	origin map[string]Origin
}

// DefaultBase is the base config path: $RECALL_CONFIG, or
// <home>/.config/recall/config.toml.
func DefaultBase(home string) string {
	if p := os.Getenv("RECALL_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(home, ".config", "recall", "config.toml")
}

// Files lists the config files in merge order: the base (if present), then
// every config.d/*.toml next to it, sorted. A missing config.d is fine.
func Files(base string) ([]string, error) {
	var files []string
	if _, err := os.Stat(base); err == nil {
		files = append(files, base)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	drops, err := filepath.Glob(filepath.Join(filepath.Dir(base), "config.d", "*.toml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(drops)
	return append(files, drops...), nil
}

// Load reads and merges the base config and its drop-ins.
func Load(base string) (*Config, error) {
	files, err := Files(base)
	if err != nil {
		return nil, err
	}
	c := &Config{Base: base, values: map[string]any{}, origin: map[string]Origin{}}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var tree map[string]any
		if err := toml.Unmarshal(data, &tree); err != nil {
			var de *toml.DecodeError
			if errors.As(err, &de) {
				row, _ := de.Position()
				return nil, fmt.Errorf("%s:%d: %s", f, row, de.Error())
			}
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		lines := keyLines(data)
		mergeInto(c.values, tree, c.origin, f, lines, "")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// mergeInto deep-merges src into dst. Tables merge key by key; anything else
// replaces the earlier value whole, clearing origins under it.
func mergeInto(dst, src map[string]any, origins map[string]Origin, file string, lines map[string]int, prefix string) {
	for k, v := range src {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		sv, srcIsMap := v.(map[string]any)
		dv, dstIsMap := dst[k].(map[string]any)
		if srcIsMap && dstIsMap {
			mergeInto(dv, sv, origins, file, lines, path)
			continue
		}
		if !srcIsMap || dstIsMap || dst[k] != nil {
			// Replacing a subtree: drop its stale origins.
			for o := range origins {
				if o == path || strings.HasPrefix(o, path+".") {
					delete(origins, o)
				}
			}
		}
		dst[k] = v
		recordOrigins(v, origins, file, lines, path)
	}
}

func recordOrigins(v any, origins map[string]Origin, file string, lines map[string]int, path string) {
	if m, ok := v.(map[string]any); ok {
		for k, mv := range m {
			recordOrigins(mv, origins, file, lines, path+"."+k)
		}
		return
	}
	origins[path] = Origin{File: file, Line: lines[path]}
}

// Get returns the merged value at a dotted key: a scalar, []any, or
// map[string]any for a table.
func (c *Config) Get(key string) (any, bool) {
	cur := any(c.values)
	for _, part := range strings.Split(key, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Origin returns where a merged leaf value was set.
func (c *Config) Origin(key string) (Origin, bool) {
	o, ok := c.origin[key]
	return o, ok
}

// Leaves returns every merged leaf key, sorted.
func (c *Config) Leaves() []string {
	keys := make([]string, 0, len(c.origin))
	for k := range c.origin {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
