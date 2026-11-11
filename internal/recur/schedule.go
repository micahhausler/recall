// Package recur reconciles declared recurring jobs with the machine:
// a marked block at the end of the crontab on Linux, one generated
// LaunchAgent per job on macOS. It owns only what it created; everything
// else in the crontab and in ~/Library/LaunchAgents is left alone.
package recur

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// field is one cron schedule position and its launchd key.
type field struct {
	name    string
	launchd string
	lo, hi  int
}

var fields = []field{
	{"minute", "Minute", 0, 59},
	{"hour", "Hour", 0, 23},
	{"dom", "Day", 1, 31},
	{"month", "Month", 1, 12},
	{"dow", "Weekday", 0, 7},
}

// NameRE limits job names and the prefix: launchd needs a label and logs
// need a file name.
var NameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

var partRE = regexp.MustCompile(`^(\*|\d+(?:-\d+)?)(?:/(\d+))?$`)

// parseField returns the sorted values a cron field matches, or nil for "*".
// Numbers, a-b, a,b, */n, and a-b/n are supported; day and month names,
// @reboot, and the other @ shortcuts are not.
func parseField(text string, lo, hi int) ([]int, error) {
	if text == "*" {
		return nil, nil
	}
	values := map[int]bool{}
	for _, part := range strings.Split(text, ",") {
		m := partRE.FindStringSubmatch(part)
		if m == nil {
			return nil, fmt.Errorf("bad field %q", text)
		}
		step := 1
		if m[2] != "" {
			step, _ = strconv.Atoi(m[2])
		}
		a, b := lo, hi
		switch {
		case m[1] == "*":
		case strings.Contains(m[1], "-"):
			lohi := strings.SplitN(m[1], "-", 2)
			a, _ = strconv.Atoi(lohi[0])
			b, _ = strconv.Atoi(lohi[1])
		default:
			a, _ = strconv.Atoi(m[1])
			b = a
			if m[2] != "" {
				b = hi
			}
		}
		if !(lo <= a && a <= b && b <= hi) || step < 1 {
			return nil, fmt.Errorf("field %q out of range %d-%d", text, lo, hi)
		}
		for v := a; v <= b; v += step {
			values[v] = true
		}
	}
	out := make([]int, 0, len(values))
	for v := range values {
		out = append(out, v)
	}
	sort.Ints(out)
	return out, nil
}

// ValidateSchedule checks five cron fields.
func ValidateSchedule(schedule string) ([]string, error) {
	parts := strings.Fields(schedule)
	if len(parts) != 5 {
		return nil, fmt.Errorf("schedule wants 5 fields, got %d", len(parts))
	}
	for i, text := range parts {
		if _, err := parseField(text, fields[i].lo, fields[i].hi); err != nil {
			return nil, err
		}
	}
	return parts, nil
}

// maxIntervals caps launchd expansion: one cron line can mean thousands of
// StartCalendarInterval dicts.
const maxIntervals = 500

// calendar converts cron fields to launchd StartCalendarInterval dicts.
// Cron ORs day-of-month and weekday when both are restricted; launchd ANDs
// keys in one dict, so that case becomes two sets of dicts.
func calendar(schedule []string) ([]map[string]int, error) {
	vals := map[string][]int{}
	for i, text := range schedule {
		v, err := parseField(text, fields[i].lo, fields[i].hi)
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		if fields[i].launchd == "Weekday" {
			folded := map[int]bool{}
			for _, x := range v {
				folded[x%7] = true
			}
			v = v[:0]
			for x := range folded {
				v = append(v, x)
			}
			sort.Ints(v)
		}
		vals[fields[i].launchd] = v
	}
	groups := []map[string][]int{vals}
	if len(vals["Day"]) > 0 && len(vals["Weekday"]) > 0 {
		groups = []map[string][]int{without(vals, "Weekday"), without(vals, "Day")}
	}
	var out []map[string]int
	for _, g := range groups {
		keys := make([]string, 0, len(g))
		for k := range g {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		combos := []map[string]int{{}}
		for _, k := range keys {
			var next []map[string]int
			for _, c := range combos {
				for _, v := range g[k] {
					n := make(map[string]int, len(c)+1)
					for ck, cv := range c {
						n[ck] = cv
					}
					n[k] = v
					next = append(next, n)
				}
			}
			combos = next
		}
		out = append(out, combos...)
	}
	if len(out) > maxIntervals {
		return nil, fmt.Errorf("schedule %q expands to %d launchd intervals", strings.Join(schedule, " "), len(out))
	}
	return out, nil
}

func without(m map[string][]int, drop string) map[string][]int {
	out := map[string][]int{}
	for k, v := range m {
		if k != drop {
			out[k] = v
		}
	}
	return out
}
