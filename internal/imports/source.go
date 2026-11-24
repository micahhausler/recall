// Package imports runs import plugins (docs/import.md): recall hands a
// configured command a request in its environment, the command writes
// sessions of recall's normalized events into a directory recall made,
// and recall validates, redacts, and stores each one under the harness's
// source. The contract is api/imports/v1; a plugin written in Go builds on
// api/imports/v1/command.
package imports

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	importsv1 "github.com/micahhausler/recall/api/imports/v1"
)

// Source runs one import: it fills spec.OutputDir with session files and
// returns the plugin's answer.
type Source interface {
	Fetch(ctx context.Context, spec importsv1.Spec) (importsv1.Status, error)
}

// stderrTail is how much of a plugin's stderr an error keeps.
const stderrTail = 64 << 10

// answerLimit bounds a plugin's stdout; the answer is one small object.
const answerLimit = 1 << 20

// Exec is the Source that runs import.command, without a shell, in the
// output directory, with an empty stdin and the request in its
// environment.
type Exec struct {
	// Command is the program and its arguments. A bare name is looked up
	// on PATH, and a path runs that file.
	Command []string
	// Timeout bounds one run; at the deadline the plugin's process group
	// is killed.
	Timeout time.Duration
	// Stderr receives the plugin's stderr a line at a time, each line
	// prefixed with Prefix; nil discards it.
	Stderr io.Writer
	Prefix string
}

func (e *Exec) Fetch(ctx context.Context, spec importsv1.Spec) (importsv1.Status, error) {
	if len(e.Command) == 0 || e.Command[0] == "" {
		return importsv1.Status{}, errors.New("no import command is configured")
	}
	name := e.Command[0]
	fail := func(tail, format string, args ...any) (importsv1.Status, error) {
		msg := fmt.Sprintf("import command %s: %s", e.Command[0], fmt.Sprintf(format, args...))
		if tail = strings.TrimSpace(tail); tail != "" {
			msg += "\n" + tail
		}
		return importsv1.Status{}, errors.New(msg)
	}
	// The plugin runs in the output directory, so a relative path is
	// resolved against recall's own first.
	if strings.ContainsRune(name, filepath.Separator) {
		abs, err := filepath.Abs(name)
		if err != nil {
			return fail("", "%v", err)
		}
		name = abs
	} else {
		path, err := exec.LookPath(name)
		if err != nil {
			return fail("", "not found on PATH: %v", err)
		}
		name = path
	}
	in, err := json.Marshal(importsv1.Import{APIVersion: importsv1.APIVersion, Kind: importsv1.Kind, Spec: &spec})
	if err != nil {
		return importsv1.Status{}, err
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, e.Command[1:]...)
	cmd.Dir = spec.OutputDir
	cmd.Env = append(os.Environ(), importsv1.InfoEnv+"="+string(in), "RECALL_HOOK=1")
	stdout := &capped{max: answerLimit}
	tail := &capped{max: stderrTail, keepTail: true}
	lines := &prefixed{w: e.Stderr, prefix: e.Prefix}
	cmd.Stdout = stdout
	cmd.Stderr = io.MultiWriter(tail, lines)
	// Its own process group, so the timeout reaches whatever it started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	if cmd.Process != nil {
		// Nothing it started may keep writing while recall reads.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	lines.flush()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fail(tail.String(), "no answer after %s", timeout)
	case err != nil:
		return fail(tail.String(), "%v", err)
	case stdout.over:
		return fail("", "printed more than %d bytes on stdout; the answer is one JSON object", answerLimit)
	}
	var answer struct {
		importsv1.Import
		Status *struct {
			Sessions *int `json:"sessions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(stdout.buf.Bytes(), &answer); err != nil {
		return fail("", "its stdout isn't one JSON object: %v", err)
	}
	if answer.APIVersion != importsv1.APIVersion || answer.Kind != importsv1.Kind {
		return fail("", "answered %q %q, want %q %q", answer.APIVersion, answer.Kind, importsv1.APIVersion, importsv1.Kind)
	}
	if answer.Status == nil || answer.Status.Sessions == nil {
		return fail("", "answered with no status.sessions")
	}
	var status struct {
		Status importsv1.Status `json:"status"`
	}
	if err := json.Unmarshal(stdout.buf.Bytes(), &status); err != nil {
		return fail("", "%v", err)
	}
	return status.Status, nil
}

// capped keeps at most max bytes: the first ones, or with keepTail the
// last ones.
type capped struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	max      int
	keepTail bool
	over     bool
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if c.keepTail {
		c.buf.Write(p)
		if extra := c.buf.Len() - c.max; extra > 0 {
			rest := append([]byte(nil), c.buf.Bytes()[extra:]...)
			c.buf.Reset()
			c.buf.Write(rest)
		}
		return n, nil
	}
	if room := c.max - c.buf.Len(); len(p) > room {
		p, c.over = p[:max(room, 0)], true
	}
	c.buf.Write(p)
	return n, nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// prefixed writes each complete line to w with prefix before it.
type prefixed struct {
	mu      sync.Mutex
	w       io.Writer
	prefix  string
	pending []byte
}

func (p *prefixed) Write(b []byte) (int, error) {
	if p.w == nil {
		return len(b), nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = append(p.pending, b...)
	for {
		i := bytes.IndexByte(p.pending, '\n')
		if i < 0 {
			break
		}
		fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.pending[:i])
		p.pending = p.pending[i+1:]
	}
	// A plugin that never ends a line still shows, a chunk at a time.
	if len(p.pending) > stderrTail {
		fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.pending)
		p.pending = nil
	}
	return len(b), nil
}

func (p *prefixed) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.w != nil && len(p.pending) > 0 {
		fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.pending)
	}
	p.pending = nil
}

// Fake is an in-memory Source for tests: it writes Files into the output
// directory and answers Status, or fails with Err after writing.
type Fake struct {
	// Files maps a file name in the output directory to its content.
	Files map[string]string
	// Prepare, when set, runs on the output directory before Files are
	// written, for entries a map can't describe.
	Prepare func(dir string) error
	Status  importsv1.Status
	Err     error
	// Specs records each call's request.
	Specs []importsv1.Spec
}

func (f *Fake) Fetch(ctx context.Context, spec importsv1.Spec) (importsv1.Status, error) {
	f.Specs = append(f.Specs, spec)
	if f.Prepare != nil {
		if err := f.Prepare(spec.OutputDir); err != nil {
			return importsv1.Status{}, err
		}
	}
	for name, body := range f.Files {
		if err := os.WriteFile(filepath.Join(spec.OutputDir, name), []byte(body), 0o600); err != nil {
			return importsv1.Status{}, err
		}
	}
	if f.Err != nil {
		return importsv1.Status{}, f.Err
	}
	return f.Status, nil
}
