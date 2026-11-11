package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/micahhausler/recall/internal/config"
)

const configUsage = `Usage:
  recall config show               the merged config, with where each value came from
  recall config get <dotted.key>   print one value (exit 1 if unset)
  recall config set <dotted.key> <value>   write one key to config.d/90-set.local.toml
`

func cmdConfig(args []string) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	base := config.DefaultBase(home)
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, configUsage)
		return 2
	}
	switch args[0] {
	case "show":
		c, err := config.Load(base)
		if err != nil {
			return fail(err)
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
		c, err := config.Load(base)
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
		if _, err := config.Load(base); err != nil {
			return fail(err)
		}
		fmt.Printf("wrote %s to %s\n", args[1], tilde(target, home))
		return 0
	default:
		fmt.Fprint(os.Stderr, configUsage)
		return 2
	}
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
	if strings.HasPrefix(path, home+"/") {
		return "~" + path[len(home):]
	}
	return path
}
