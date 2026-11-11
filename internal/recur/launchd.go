package recur

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/micahhausler/recall/internal/config"
)

func init() {
	Register("launchd", func(s Setup) Scheduler { return &launchd{s} })
}

// launchd owns one generated LaunchAgent per job, <prefix>.<name>.plist.
// The plists are build output, never something to commit. The `launchctl`
// command comes from PATH, so tests install a fake.
type launchd struct {
	Setup
}

// AgentsDir is where LaunchAgents live under a home.
func AgentsDir(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents")
}

// StateDir is where job logs live under a home.
func StateDir(home string) string {
	return filepath.Join(home, ".local", "state", "recall", "recur")
}

func (l *launchd) plist(j config.Job) (label string, data []byte, err error) {
	label = l.Prefix + "." + j.Name
	sched, err := ValidateSchedule(j.Schedule)
	if err != nil {
		return "", nil, fmt.Errorf("job %q: %v", j.Name, err)
	}
	cal, err := calendar(sched)
	if err != nil {
		return "", nil, fmt.Errorf("job %q: %v", j.Name, err)
	}
	log := filepath.Join(StateDir(l.Home), j.Name+".launchd.log")
	d := map[string]any{
		"Label":                 label,
		"ProgramArguments":      l.runArgs(j.Name),
		"StartCalendarInterval": cal,
		"StandardOutPath":       log,
		"StandardErrorPath":     log,
		"ProcessType":           "Background",
	}
	if l.ConfigEnv != "" {
		d["EnvironmentVariables"] = map[string]any{"RECALL_CONFIG": l.ConfigEnv}
	}
	return label, marshalPlist(d), nil
}

func (l *launchd) domain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func launchctl(args ...string) (int, string) {
	cmd := exec.Command("launchctl", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	return 127, err.Error()
}

func (l *launchd) Sync(jobs map[string]config.Job, check bool, out io.Writer) ([]Action, error) {
	agents := AgentsDir(l.Home)
	want := map[string][]byte{}
	sched := map[string]string{}
	for _, j := range sortedJobs(jobs) {
		label, data, err := l.plist(j)
		if err != nil {
			return nil, err
		}
		want[label] = data
		sched[label] = j.Schedule
	}
	have := map[string]string{}
	if entries, err := os.ReadDir(agents); err == nil {
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, l.Prefix+".") && strings.HasSuffix(n, ".plist") {
				have[strings.TrimSuffix(n, ".plist")] = filepath.Join(agents, n)
			}
		}
	}

	type step struct {
		Action
		path string
		data []byte
	}
	var plan []step
	labels := make([]string, 0, len(want))
	for label := range want {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		path := filepath.Join(agents, label+".plist")
		var current []byte
		if _, ok := have[label]; ok {
			current, _ = os.ReadFile(path)
		}
		rc, _ := launchctl("print", l.domain()+"/"+label)
		loaded := rc == 0
		switch {
		case string(current) != string(want[label]):
			op := "add"
			if current != nil {
				op = "update"
			}
			plan = append(plan, step{Action{op, label}, path, want[label]})
		case !loaded:
			plan = append(plan, step{Action{"load", label}, path, want[label]})
		case check:
			fmt.Fprintf(out, "ok     %s  %s  (loaded, %s)\n", label, sched[label], tildePath(path, l.Home))
		}
	}
	removes := make([]string, 0, len(have))
	for label := range have {
		if _, ok := want[label]; !ok {
			removes = append(removes, label)
		}
	}
	sort.Strings(removes)
	for _, label := range removes {
		plan = append(plan, step{Action{"remove", label}, have[label], nil})
	}

	var actions []Action
	for _, st := range plan {
		actions = append(actions, st.Action)
		fmt.Fprintf(out, "%-6s %s\n", st.Op, st.What)
		if check {
			continue
		}
		launchctl("bootout", l.domain()+"/"+st.What)
		if st.Op == "remove" {
			os.Remove(st.path)
			continue
		}
		if err := os.MkdirAll(agents, 0o755); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(StateDir(l.Home), 0o755); err != nil {
			return nil, err
		}
		if st.Op != "load" {
			if err := os.WriteFile(st.path+".tmp", st.data, 0o644); err != nil {
				return nil, err
			}
			if err := os.Rename(st.path+".tmp", st.path); err != nil {
				return nil, err
			}
		}
		if rc, msg := launchctl("bootstrap", l.domain(), st.path); rc != 0 {
			return nil, fmt.Errorf("launchctl bootstrap %s: %s", st.What, strings.TrimSpace(msg))
		}
	}
	return actions, nil
}

func tildePath(path, home string) string {
	if strings.HasPrefix(path, home+"/") {
		return "~" + path[len(home):]
	}
	return path
}

// marshalPlist writes a launchd property list with sorted keys, so generated
// plists compare byte for byte.
func marshalPlist(v any) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	writePlistValue(&b, v, 0)
	b.WriteString("</plist>\n")
	return []byte(b.String())
}

func writePlistValue(b *strings.Builder, v any, depth int) {
	ind := strings.Repeat("\t", depth)
	switch tv := v.(type) {
	case string:
		fmt.Fprintf(b, "%s<string>%s</string>\n", ind, xmlEscape(tv))
	case int:
		fmt.Fprintf(b, "%s<integer>%d</integer>\n", ind, tv)
	case []string:
		anyv := make([]any, len(tv))
		for i, s := range tv {
			anyv[i] = s
		}
		writePlistValue(b, anyv, depth)
	case []map[string]int:
		anyv := make([]any, len(tv))
		for i, m := range tv {
			am := make(map[string]any, len(m))
			for k, x := range m {
				am[k] = x
			}
			anyv[i] = am
		}
		writePlistValue(b, anyv, depth)
	case []any:
		fmt.Fprintf(b, "%s<array>\n", ind)
		for _, e := range tv {
			writePlistValue(b, e, depth+1)
		}
		fmt.Fprintf(b, "%s</array>\n", ind)
	case map[string]any:
		fmt.Fprintf(b, "%s<dict>\n", ind)
		keys := make([]string, 0, len(tv))
		for k := range tv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(b, "%s\t<key>%s</key>\n", ind, xmlEscape(k))
			writePlistValue(b, tv[k], depth+1)
		}
		fmt.Fprintf(b, "%s</dict>\n", ind)
	default:
		panic(fmt.Sprintf("plist: unhandled type %T", v))
	}
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
