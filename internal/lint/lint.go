// Package lint finds things context files mention that don't exist on this
// machine: paths, script-like commands, and skill names. It prefers false
// negatives to noisy false positives, so it only checks tokens that clearly
// look like one of those.
package lint

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Finding is one stale mention.
type Finding struct {
	File    string
	Line    int
	Message string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Message)
}

// Linter checks context files.
type Linter struct {
	Home string
	// Names are the skills and slash commands that exist on this machine;
	// a backticked /name is checked against them. Empty means skip the
	// check.
	Names map[string]bool
	// LookPath finds a command; exec.LookPath normally, a fake in tests.
	LookPath func(string) (string, error)
	// Ignore lists absolute paths never reported, such as a side file that
	// sparse checkout deliberately keeps off this machine.
	Ignore map[string]bool
}

var backtickRE = regexp.MustCompile("`([^`\n]+)`")
var importRE = regexp.MustCompile(`^@(\S+)$`)
var slashRE = regexp.MustCompile(`^/[a-z0-9][a-z0-9-]*$`)
var commandishRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// Run lints the files and every @import they pull in, depth first.
func (l *Linter) Run(files []string) ([]Finding, error) {
	if l.LookPath == nil {
		l.LookPath = exec.LookPath
	}
	seen := map[string]bool{}
	var findings []Finding
	var walk func(path string) error
	walk = func(path string) error {
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if seen[abs] {
			return nil
		}
		seen[abs] = true
		f, err := os.Open(abs)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		n := 0
		var imports []string
		for sc.Scan() {
			n++
			line := sc.Text()
			if m := importRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				target := l.expand(m[1])
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(abs), target)
				}
				if _, err := os.Stat(target); err != nil {
					if !l.Ignore[target] {
						findings = append(findings, Finding{abs, n, fmt.Sprintf("import @%s: %s doesn't exist", m[1], target)})
					}
				} else {
					imports = append(imports, target)
				}
				continue
			}
			findings = append(findings, l.lintLine(abs, n, line)...)
		}
		if err := sc.Err(); err != nil {
			return err
		}
		for _, imp := range imports {
			if err := walk(imp); err != nil {
				return err
			}
		}
		return nil
	}
	for _, path := range files {
		if err := walk(path); err != nil {
			return nil, err
		}
	}
	return findings, nil
}

func (l *Linter) lintLine(file string, n int, line string) []Finding {
	var out []Finding
	for _, m := range backtickRE.FindAllStringSubmatch(line, -1) {
		token := m[1]
		if f := l.check(token); f != "" {
			out = append(out, Finding{file, n, f})
		}
	}
	return out
}

// check returns a message when a token clearly names something that doesn't
// exist, and "" otherwise. Tokens with placeholders, globs, or variables
// other than $HOME are skipped.
func (l *Linter) check(token string) string {
	if strings.ContainsAny(token, "*?<>[]()|$") && !strings.Contains(token, "$HOME") {
		return ""
	}
	// `/name`: a skill or slash command, unless it's a path that exists,
	// like `/tmp`.
	if slashRE.MatchString(token) {
		if _, err := os.Stat(token); err == nil {
			return ""
		}
		name := token[1:]
		if len(l.Names) == 0 || l.Names[name] {
			return ""
		}
		return fmt.Sprintf("skill %s: no skill or command named %q on this machine", token, name)
	}
	first, _, hasArgs := strings.Cut(token, " ")
	// `~/path` or `/path`, alone or as a command's first word.
	if strings.HasPrefix(first, "~/") || strings.HasPrefix(first, "/") || strings.HasPrefix(first, "$HOME/") {
		p := l.expand(first)
		if !strings.HasPrefix(p, "/") {
			return ""
		}
		if _, err := os.Stat(p); err != nil && !l.Ignore[p] {
			return fmt.Sprintf("path %s doesn't exist", first)
		}
		return ""
	}
	// A command's first word, only when it looks script-like (a hyphen, so
	// plain words in prose never match), e.g. `jobs-sync --check`.
	if hasArgs && strings.Contains(first, "-") && commandishRE.MatchString(first) && !strings.Contains(first, "/") {
		if _, err := l.LookPath(first); err != nil {
			return fmt.Sprintf("command %s not on PATH", first)
		}
	}
	return ""
}

func (l *Linter) expand(p string) string {
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		return filepath.Join(l.Home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	case strings.HasPrefix(p, "$HOME/"):
		return filepath.Join(l.Home, p[len("$HOME/"):])
	}
	return p
}
