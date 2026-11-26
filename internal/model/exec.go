package model

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// ScratchDir is an empty directory for a run that needs no files, and the
// func that removes it.
func ScratchDir() (string, func(), error) {
	dir, err := os.MkdirTemp("", "recall-run-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

// Cmd is one headless harness run.
type Cmd struct {
	Command string
	Args    []string
	Dir     string
	// Unset are environment variables the run drops.
	Unset []string
	Input string
}

// Run runs c with Input on stdin and HookGuard set, and returns its
// stdout, its trimmed stderr, and its exit error.
func (c Cmd) Run(ctx context.Context) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, c.Command, c.Args...)
	cmd.Dir = c.Dir
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(c.Unset, name)
	})
	cmd.Env = append(env, HookGuard)
	cmd.Stdin = strings.NewReader(c.Input)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	return out.Bytes(), strings.TrimSpace(errOut.String()), err
}
