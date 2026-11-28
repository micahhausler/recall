// Command gendocs renders docs/commands.md from the built recall binary's
// help, and docs/config.md from its template and the config schema's
// defaults. TestDocsAreGenerated fails when a checked-in file differs.
package main

//go:generate go run . -root ../..

import (
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/micahhausler/recall/internal/config"
)

var (
	//go:embed commands.md.tmpl
	commandsTmpl string
	//go:embed config.md.tmpl
	configTmpl string
)

// Placeholders the generated docs show for this machine.
const (
	docHome = "/home/you"
	docHost = "laptop-a"
)

func main() {
	root := flag.String("root", ".", "the repo root")
	flag.Parse()
	docs, err := render(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gendocs: %v\n", err)
		os.Exit(1)
	}
	for name, text := range docs {
		if err := os.WriteFile(filepath.Join(*root, name), text, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "gendocs: %v\n", err)
			os.Exit(1)
		}
	}
}

// render is every generated file, by its path under root.
func render(root string) (map[string][]byte, error) {
	tmp, err := os.MkdirTemp("", "gendocs")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	bin := filepath.Join(tmp, "recall")
	build := exec.Command("go", "build", "-o", bin, "./cmd/recall")
	build.Dir, build.Stderr = root, os.Stderr
	if err := build.Run(); err != nil {
		return nil, fmt.Errorf("build recall: %w", err)
	}
	cmds, err := commandsOf(bin, tmp)
	if err != nil {
		return nil, err
	}
	commands, err := execute(commandsTmpl, struct {
		Commands, People []command
	}{cmds, visible(cmds)})
	if err != nil {
		return nil, err
	}
	keys, err := config.Defaults(nil, docHome, docHost)
	if err != nil {
		return nil, err
	}
	cfg, err := execute(configTmpl, struct {
		Keys        []config.Default
		Host, Unset string
	}{keys, docHost, config.Unset})
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		filepath.Join("docs", "commands.md"): commands,
		filepath.Join("docs", "config.md"):   cfg,
	}, nil
}

// command is one top-level command as `recall --help --all` lists it.
type command struct {
	Name    string
	Visible bool
	Lines   []line
	// Usage is `recall <name> -h`.
	Usage string
}

type line struct{ Usage, Help string }

// helpColumn is where main.go's usage pads a command's synopsis to.
const helpColumn = 28

// commandsOf reads the command table from the binary's help, and each
// command's usage, run with an empty home so no config is read.
func commandsOf(bin, home string) ([]command, error) {
	help, err := runHelp(bin, home, "--help", "--all")
	if err != nil {
		return nil, err
	}
	var out []command
	section := 0 // 1 for the people's list, 2 for the hidden one
	for _, l := range strings.Split(help, "\n") {
		switch {
		case l == "Usage:":
			section = 1
			continue
		case strings.HasPrefix(l, "Called by "):
			section = 2
			continue
		case !strings.HasPrefix(l, "  recall "):
			continue
		}
		if section == 0 {
			continue
		}
		rest := strings.TrimPrefix(l, "  recall ")
		if len(rest) <= helpColumn || rest[helpColumn] != ' ' {
			return nil, fmt.Errorf("help line %q: synopsis wider than %d columns", l, helpColumn)
		}
		ln := line{Usage: strings.TrimSpace(rest[:helpColumn]), Help: strings.TrimSpace(rest[helpColumn:])}
		name, _, _ := strings.Cut(ln.Usage, " ")
		if n := len(out); n > 0 && out[n-1].Name == name {
			out[n-1].Lines = append(out[n-1].Lines, ln)
			continue
		}
		out = append(out, command{Name: name, Visible: section == 1, Lines: []line{ln}})
	}
	if len(out) == 0 {
		return nil, errors.New("recall --help --all listed no commands")
	}
	for i := range out {
		u, err := runHelp(bin, home, out[i].Name, "-h")
		if err != nil {
			return nil, err
		}
		out[i].Usage = strings.TrimRight(u, "\n")
	}
	return out, nil
}

// runHelp is a help command's output; help exits 0 or 2.
func runHelp(bin, home string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), config.EnvVar + "=" + filepath.Join(home, "none.toml")}
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 {
		err = nil
	}
	if err != nil {
		return "", fmt.Errorf("recall %s: %w", strings.Join(args, " "), err)
	}
	return b.String(), nil
}

func visible(cmds []command) []command {
	var out []command
	for _, c := range cmds {
		if c.Visible {
			out = append(out, c)
		}
	}
	return out
}

var funcs = template.FuncMap{
	"cell": func(s string) string { return strings.ReplaceAll(s, "|", `\|`) },
	"names": func(cmds []command) string {
		var out []string
		for _, c := range cmds {
			out = append(out, "`"+c.Name+"`")
		}
		if n := len(out); n > 2 {
			out[n-1] = "and " + out[n-1]
			return strings.Join(out, ", ")
		}
		return strings.Join(out, " and ")
	},
}

func execute(text string, data any) ([]byte, error) {
	t, err := template.New("doc").Funcs(funcs).Parse(text)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
