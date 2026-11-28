package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/micahhausler/recall/internal/config"
	"golang.org/x/term"
)

const configUsage = `Usage:
  recall config show               the merged config, with where each value came from
  recall config show --defaults    every key with the value recall uses on this machine, marking the keys a file sets
  recall config get <dotted.key>   print one value (exit 1 if unset)
  recall config set <dotted.key> <value>   write one key to config.d/90-set.local.toml
`

func cmdConfig(args []string) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	home = filepath.Clean(home)
	base := config.DefaultBase(home)
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, configUsage)
		return 2
	}
	switch args[0] {
	case "show":
		defaults := len(args) == 2 && args[1] == "--defaults"
		if len(args) > 1 && !defaults {
			fmt.Fprint(os.Stderr, configUsage)
			return 2
		}
		c, err := loadConfig(base)
		if err != nil {
			return fail(err)
		}
		if defaults {
			hostname, _ := osHostname()
			if err := showDefaults(os.Stdout, c, home, shortHost(hostname), term.IsTerminal(int(os.Stdout.Fd()))); err != nil {
				return fail(err)
			}
			return 0
		}
		leaves := c.Leaves()
		width := 0
		lines := make([]string, len(leaves))
		for i, key := range leaves {
			v, _ := c.Get(key)
			lines[i] = fmt.Sprintf("%s = %s", key, formatValue(v))
			if len(lines[i]) > width {
				width = len(lines[i])
			}
		}
		for i, key := range leaves {
			o, _ := c.Origin(key)
			fmt.Printf("%-*s  %s\n", width, lines[i], tilde(o.String(), home))
		}
		return 0
	case "get":
		if len(args) != 2 {
			fmt.Fprint(os.Stderr, configUsage)
			return 2
		}
		c, err := loadConfig(base)
		if err != nil {
			return fail(err)
		}
		v, ok := c.Get(args[1])
		if !ok {
			return 1
		}
		switch tv := v.(type) {
		case []any:
			for _, e := range tv {
				fmt.Println(plainValue(e))
			}
		case map[string]any:
			keys := make([]string, 0, len(tv))
			for k := range tv {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Println(strings.Join(keys, "\n"))
		default:
			fmt.Println(plainValue(v))
		}
		return 0
	case "set":
		if len(args) != 3 {
			fmt.Fprint(os.Stderr, configUsage)
			return 2
		}
		target, err := config.Set(base, args[1], args[2])
		if err != nil {
			return fail(err)
		}
		// Reload so a bad value is caught now, not on the next run.
		if _, err := loadConfig(base); err != nil {
			return fail(err)
		}
		fmt.Printf("wrote %s to %s\n", args[1], tilde(target, home))
		return 0
	default:
		fmt.Fprint(os.Stderr, configUsage)
		return 2
	}
}

// osHostname is os.Hostname; tests replace it.
var osHostname = os.Hostname

// showDefaults prints every schema key with the value recall uses, and each
// key a file sets with its value and origin, bold when bold is set and
// marked * otherwise.
func showDefaults(w io.Writer, c *config.Config, home, hostname string, bold bool) error {
	defs, err := config.Defaults(c, home, hostname)
	if err != nil {
		return err
	}
	type row struct{ text, origin string }
	var rows []row
	setRow := func(key string) row {
		v, _ := c.Get(key)
		o, _ := c.Origin(key)
		return row{fmt.Sprintf("%s = %s", key, formatValue(v)), tilde(o.String(), home)}
	}
	leaves := c.Leaves()
	printed := map[string]bool{}
	for i, d := range defs {
		if _, ok := c.Origin(d.Key); ok {
			rows = append(rows, setRow(d.Key))
		} else {
			rows = append(rows, row{text: fmt.Sprintf("%s = %s", d.Key, d.Value)})
		}
		printed[d.Key] = true
		if d.Key == d.Pattern || (i+1 < len(defs) && defs[i+1].Pattern == d.Pattern) {
			continue
		}
		// The rest of the pattern's instances a file sets.
		for _, key := range leaves {
			if d.Matches(key) && !printed[key] {
				rows = append(rows, setRow(key))
				printed[key] = true
			}
		}
	}
	width := 0
	for _, r := range rows {
		if r.origin != "" && len(r.text) > width {
			width = len(r.text)
		}
	}
	for _, r := range rows {
		switch {
		case r.origin == "" && bold:
			fmt.Fprintln(w, r.text)
		case r.origin == "":
			fmt.Fprintln(w, "  "+r.text)
		case bold:
			fmt.Fprintf(w, "\x1b[1m%-*s  %s\x1b[0m\n", width, r.text, r.origin)
		default:
			fmt.Fprintf(w, "* %-*s  %s\n", width, r.text, r.origin)
		}
	}
	return nil
}

// warnedKeys holds the ignored keys already warned about in this process.
var (
	warnedMu   sync.Mutex
	warnedKeys = map[string]bool{}
)

func resetConfigWarnings() {
	warnedMu.Lock()
	defer warnedMu.Unlock()
	warnedKeys = map[string]bool{}
}

// loadConfig loads the config and warns on stderr, once per key per
// process, about each key it ignored.
func loadConfig(base string) (*config.Config, error) {
	c, err := config.Load(base)
	if err != nil {
		return nil, err
	}
	warnedMu.Lock()
	defer warnedMu.Unlock()
	for _, w := range c.Warnings() {
		if warnedKeys[w.Key] {
			continue
		}
		warnedKeys[w.Key] = true
		fmt.Fprintf(os.Stderr, "recall: %s\n", w)
	}
	return c, nil
}

func formatValue(v any) string {
	switch tv := v.(type) {
	case string:
		return fmt.Sprintf("%q", tv)
	case []any:
		parts := make([]string, len(tv))
		for i, e := range tv {
			parts[i] = formatValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return fmt.Sprintf("%v", v)
	}
}

func plainValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func tilde(path, home string) string {
	home = filepath.Clean(home)
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+"/") {
		return "~" + path[len(home):]
	}
	return path
}
