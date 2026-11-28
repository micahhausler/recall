package config

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Unset is the value of a key with no default.
const Unset = "(unset)"

// NamePlaceholder stands in for a wildcard segment.
const NamePlaceholder = "<name>"

// Default is one schema pattern and its default as text.
type Default struct {
	// Pattern is the schema pattern; * matches one name segment.
	Pattern string
	// Key is Pattern with NamePlaceholder in place of *.
	Key string
	// Value is the default as TOML, or Unset.
	Value string
}

// Matches reports whether key is an instance of d's pattern.
func (d Default) Matches(key string) bool {
	return matches(d.Pattern, key)
}

// segmentFields names the Settings fields whose names don't follow their key segment.
var segmentFields = map[string]string{
	"archive-chats": "Archive",
	"harness":       "Harnesses",
}

// wildcardDefaults are instance defaults no empty instance can show.
var wildcardDefaults = map[string]func() string{
	"harness.*.import.timeout": func() string { return fmt.Sprintf("%q", shortDuration(DefaultImportTimeout)) },
	"recur.jobs.*.enabled":     func() string { return fmt.Sprint(DefaultJobEnabled) },
}

// probeSeeds are the smallest instance under each wildcard prefix that
// loads; any other prefix gets an empty table.
var probeSeeds = map[string]func() any{
	"recur.jobs": func() any { return map[string]any{"enabled": false} },
	"extensions": func() any { return []any{} },
}

// Defaults returns every schema pattern, in schema order, with the value
// Settings gives it for c (nil for an empty config), which for a key c
// doesn't set is the value recall uses on this machine. Each known
// harness's built-in source follows harness.<name>.source. Home prints as ~.
func Defaults(c *Config, home, hostname string) ([]Default, error) {
	home = filepath.Clean(home)
	if c == nil {
		c = emptyConfig(nil)
	}
	loaded, err := c.Settings(home, hostname)
	if err != nil {
		return nil, err
	}
	probes := map[string]*Settings{}
	out := make([]Default, 0, len(schema))
	for _, p := range schema {
		segs := strings.Split(p, ".")
		s := loaded
		if i := slices.Index(segs, "*"); i >= 0 {
			prefix := strings.Join(segs[:i], ".")
			if _, ok := probes[prefix]; !ok {
				ps, err := c.probe(segs[:i], home, hostname)
				if err != nil {
					return nil, fmt.Errorf("defaults for %s: %w", p, err)
				}
				probes[prefix] = ps
			}
			s = probes[prefix]
		}
		value := Unset
		if f, ok := wildcardDefaults[p]; ok {
			value = f()
		} else if v, ok := fieldValue(reflect.ValueOf(*s), segs); ok {
			value = formatDefault(v, home)
		}
		out = append(out, Default{Pattern: p, Key: strings.ReplaceAll(p, "*", NamePlaceholder), Value: value})
		if p == "harness.*.source" {
			for _, name := range KnownHarnesses {
				out = append(out, Default{Pattern: p, Key: "harness." + name + ".source",
					Value: formatDefault(reflect.ValueOf(DefaultSource(name, home)), home)})
			}
		}
	}
	return out, nil
}

func emptyConfig(values map[string]any) *Config {
	if values == nil {
		values = map[string]any{}
	}
	return &Config{values: values, origin: map[string]Origin{}, tables: map[string]Origin{}}
}

// probe is Settings on a copy of c that declares one more instance,
// NamePlaceholder, under prefix.
func (c *Config) probe(prefix []string, home, hostname string) (*Settings, error) {
	values := copyTree(c.values)
	cur := values
	for _, seg := range prefix {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	seed := any(map[string]any{})
	if f, ok := probeSeeds[strings.Join(prefix, ".")]; ok {
		seed = f()
	}
	cur[NamePlaceholder] = seed
	return (&Config{values: values, origin: c.origin, tables: c.tables}).Settings(home, hostname)
}

func copyTree(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			v = copyTree(sub)
		}
		out[k] = v
	}
	return out
}

// field finds the struct field a key segment names.
func field(t reflect.Type, seg string) (reflect.StructField, bool) {
	if name, ok := segmentFields[seg]; ok {
		return t.FieldByName(name)
	}
	want := strings.ReplaceAll(seg, "_", "")
	for i := 0; i < t.NumField(); i++ {
		if f := t.Field(i); strings.EqualFold(f.Name, want) {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// fieldType walks a pattern through Settings' types.
func fieldType(t reflect.Type, segs []string) (reflect.Type, bool) {
	for _, seg := range segs {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Map:
			t = t.Elem()
		case reflect.Struct:
			f, ok := field(t, seg)
			if !ok {
				return nil, false
			}
			t = f.Type
		default:
			return nil, false
		}
	}
	return t, true
}

// fieldValue walks a pattern through a Settings value; * reads NamePlaceholder.
func fieldValue(v reflect.Value, segs []string) (reflect.Value, bool) {
	for _, seg := range segs {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		switch v.Kind() {
		case reflect.Map:
			if seg == "*" {
				seg = NamePlaceholder
			}
			v = v.MapIndex(reflect.ValueOf(seg))
			if !v.IsValid() {
				return reflect.Value{}, false
			}
		case reflect.Struct:
			f, ok := field(v.Type(), seg)
			if !ok {
				return reflect.Value{}, false
			}
			v = v.FieldByIndex(f.Index)
		default:
			return reflect.Value{}, false
		}
	}
	return v, true
}

func formatDefault(v reflect.Value, home string) string {
	if d, ok := v.Interface().(time.Duration); ok {
		return fmt.Sprintf("%q", shortDuration(d))
	}
	switch v.Kind() {
	case reflect.String:
		if v.String() == "" {
			return Unset
		}
		return fmt.Sprintf("%q", tildeHome(v.String(), home))
	case reflect.Bool, reflect.Int, reflect.Int64, reflect.Float64:
		return fmt.Sprintf("%v", v.Interface())
	case reflect.Slice:
		if v.Len() == 0 {
			return Unset
		}
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = formatDefault(v.Index(i), home)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return Unset
}

// shortDuration drops the zero units time.Duration prints: 10m, not 10m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func tildeHome(path, home string) string {
	home = filepath.Clean(home)
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+"/") {
		return "~" + path[len(home):]
	}
	return path
}
