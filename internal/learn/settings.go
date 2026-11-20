package learn

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A rule a harness can enforce should graduate into the harness's own
// settings: a hook that runs a check on every matching tool call, or a
// permission rule that denies or gates a command. Then no prose is left for
// the agent to remember.
//
// The binary, not the apply model, edits the settings file, and only in
// these two shapes: it adds a command hook that runs a script under
// learn.bin, or a deny or ask rule. It never adds an allow rule. A loop
// that learns from sessions must not be able to widen what the agent may
// do, even through a reviewed change.

// Enforcement is how a graduate proposal asks for its rule to be enforced.
type Enforcement struct {
	// Kind is script (the default), hook, or permission.
	Kind string
	// Event and Matcher place a hook, like PreToolUse and Bash.
	Event, Matcher string
	// Verb and Rule are a permission rule: deny or ask, and its pattern,
	// like Bash(git push --force:*).
	Verb, Rule string
}

var (
	hookEvents    = map[string]bool{"PreToolUse": true, "PostToolUse": true, "Stop": true}
	matcherRE     = regexp.MustCompile(`^[A-Za-z0-9_|.*-]+$`)
	permissionRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\(.+\))?$`)
	enforceScript = "script"
)

// parseEnforcement reads a graduate proposal's "- enforce:", "- hook:", and
// "- permission:" values. An empty why means it is well formed.
func parseEnforcement(enforce, hook, permission string) (e Enforcement, why string) {
	clean := func(s string) string { return strings.Trim(strings.TrimSpace(s), "`\"") }
	e.Kind = strings.ToLower(clean(enforce))
	switch e.Kind {
	case "", enforceScript:
		e.Kind = enforceScript
	case "hook":
		fields := strings.Fields(clean(hook))
		if len(fields) == 0 || len(fields) > 2 {
			return e, fmt.Sprintf("hook %q isn't an event and a tool matcher, like \"PreToolUse Bash\"", hook)
		}
		e.Event = fields[0]
		if !hookEvents[e.Event] {
			return e, fmt.Sprintf("hook event %q isn't PreToolUse, PostToolUse, or Stop", e.Event)
		}
		if len(fields) == 2 {
			e.Matcher = fields[1]
			if !matcherRE.MatchString(e.Matcher) {
				return e, fmt.Sprintf("hook matcher %q isn't a tool name pattern", e.Matcher)
			}
		}
	case "permission":
		verb, rule, _ := strings.Cut(clean(permission), " ")
		e.Verb, e.Rule = strings.ToLower(verb), strings.TrimSpace(rule)
		if e.Verb == "allow" {
			return e, "a learned rule may deny or ask, never allow: recall doesn't widen what the agent may do"
		}
		if e.Verb != "deny" && e.Verb != "ask" {
			return e, fmt.Sprintf("permission %q doesn't start with deny or ask", permission)
		}
		if !permissionRE.MatchString(e.Rule) {
			return e, fmt.Sprintf("permission rule %q isn't a tool pattern, like Bash(git push:*)", e.Rule)
		}
	default:
		return e, fmt.Sprintf("enforce %q isn't script, hook, or permission", enforce)
	}
	return e, ""
}

// member is one key of a JSON object with its value's bytes as written.
type member struct {
	key string
	raw json.RawMessage
}

// ordered is a JSON object that keeps its keys in order, so rewriting part
// of a settings file doesn't reshuffle the rest.
type ordered struct {
	members []member
}

// get and set read and write the last member with a key: when a file
// repeats a key, the last one is the one a JSON parser keeps.
func (o *ordered) get(key string) (json.RawMessage, bool) {
	for i := len(o.members) - 1; i >= 0; i-- {
		if o.members[i].key == key {
			return o.members[i].raw, true
		}
	}
	return nil, false
}

func (o *ordered) set(key string, raw json.RawMessage) {
	for i := len(o.members) - 1; i >= 0; i-- {
		if o.members[i].key == key {
			o.members[i].raw = raw
			return
		}
	}
	o.members = append(o.members, member{key, raw})
}

// parseObject reads a JSON object's keys, in order, with each value's bytes
// untouched. Empty input is an empty object.
func parseObject(data []byte) (*ordered, error) {
	o := &ordered{}
	if len(bytes.TrimSpace(data)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("not a JSON object")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		o.members = append(o.members, member{key, raw})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	// Anything after the object would be dropped by a rewrite.
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("there is content after the JSON object")
	}
	return o, nil
}

// encode writes v as JSON without escaping <, >, or &, which a shell
// command in a hook is full of.
func encode(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSpace(buf.Bytes())
}

// render writes the object at a nesting depth, one key per line. A value
// this code didn't touch is written back byte for byte.
func (o *ordered) render(indent string, depth int) json.RawMessage {
	if len(o.members) == 0 {
		return json.RawMessage("{}")
	}
	pad := strings.Repeat(indent, depth+1)
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, m := range o.members {
		b.WriteString(pad)
		b.Write(encode(m.key))
		b.WriteString(": ")
		b.Write(m.raw)
		if i < len(o.members)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(strings.Repeat(indent, depth) + "}")
	return b.Bytes()
}

var indentRE = regexp.MustCompile(`\n([ \t]+)"`)

// indentOf is the indentation a JSON file already uses, two spaces when it
// shows none.
func indentOf(data []byte) string {
	if m := indentRE.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return "  "
}

// list renders a JSON array one item per line at a nesting depth.
func list(items []json.RawMessage, indent string, depth int) json.RawMessage {
	if len(items) == 0 {
		return json.RawMessage("[]")
	}
	pad := strings.Repeat(indent, depth+1)
	var b bytes.Buffer
	b.WriteString("[\n")
	for i, item := range items {
		b.WriteString(pad)
		b.Write(item)
		if i < len(items)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(strings.Repeat(indent, depth) + "]")
	return b.Bytes()
}

func items(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Enforce adds one enforcement to a harness settings file: a command hook
// that runs command, or a permission rule. Everything else in the file is
// left as written, in its order. Adding what is already there changes
// nothing. A missing file is created.
func Enforce(path string, e Enforcement, command string) error {
	// Checked here too, not only where a proposal is parsed: this is the
	// one function that writes a settings file, and it must not be able to
	// write an allow rule or an unknown hook whoever calls it.
	switch {
	case e.Kind == "hook" && (!hookEvents[e.Event] || (e.Matcher != "" && !matcherRE.MatchString(e.Matcher)) || command == ""):
		return fmt.Errorf("refusing hook %q %q: not a known event with a tool matcher and a command", e.Event, e.Matcher)
	case e.Kind == "permission" && ((e.Verb != "deny" && e.Verb != "ask") || !permissionRE.MatchString(e.Rule)):
		return fmt.Errorf("refusing permission %q %q: only a deny or ask rule for a tool pattern", e.Verb, e.Rule)
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	root, err := parseObject(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	indent := indentOf(data)
	switch e.Kind {
	case "hook":
		raw, _ := root.get("hooks")
		hooks, err := parseObject(raw)
		if err != nil {
			return fmt.Errorf("%s: hooks: %w", path, err)
		}
		raw, _ = hooks.get(e.Event)
		groups, err := items(raw)
		if err != nil {
			return fmt.Errorf("%s: hooks.%s: %w", path, e.Event, err)
		}
		for _, g := range groups {
			var have struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			}
			if json.Unmarshal(g, &have) == nil && have.Matcher == e.Matcher {
				for _, h := range have.Hooks {
					if h.Command == command {
						return nil
					}
				}
			}
		}
		group := &ordered{}
		if e.Matcher != "" {
			group.set("matcher", encode(e.Matcher))
		}
		hook := &ordered{}
		hook.set("type", encode("command"))
		hook.set("command", encode(command))
		group.set("hooks", list([]json.RawMessage{hook.render(indent, 5)}, indent, 4))
		groups = append(groups, group.render(indent, 3))
		hooks.set(e.Event, list(groups, indent, 2))
		root.set("hooks", hooks.render(indent, 1))
	case "permission":
		raw, _ := root.get("permissions")
		perms, err := parseObject(raw)
		if err != nil {
			return fmt.Errorf("%s: permissions: %w", path, err)
		}
		raw, _ = perms.get(e.Verb)
		rules, err := items(raw)
		if err != nil {
			return fmt.Errorf("%s: permissions.%s: %w", path, e.Verb, err)
		}
		want := encode(e.Rule)
		for _, r := range rules {
			if bytes.Equal(bytes.TrimSpace(r), want) {
				return nil
			}
		}
		perms.set(e.Verb, list(append(rules, want), indent, 2))
		root.set("permissions", perms.render(indent, 1))
	default:
		return fmt.Errorf("nothing to add to %s for %q", path, e.Kind)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(root.render(indent, 0), '\n'), 0o644)
}
