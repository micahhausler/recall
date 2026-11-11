// Package model runs prompts through a harness CLI in headless mode (for
// example `claude -p`), never an API client, so existing auth, billing, and
// model selection work unchanged.
package model

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner answers one prompt with one completion.
type Runner interface {
	// Run sends input to the model named by model and returns its text.
	Run(ctx context.Context, model, input string) (string, error)
}

// CLI runs a harness command headless: `<command> -p --model <model>` with
// the input on stdin. Every tool is disallowed, so the run reads stdin and
// writes stdout and nothing else.
type CLI struct {
	// Command is the harness binary, default "claude".
	Command string
}

func (c *CLI) Run(ctx context.Context, model, input string) (string, error) {
	command := c.Command
	if command == "" {
		command = "claude"
	}
	args := []string{"-p", "--model", model, "--disallowedTools", "*"}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = strings.NewReader(input)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errOut.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%s -p: %s", command, detail)
	}
	return out.String(), nil
}

// Fake is a Runner for tests: canned responses, recorded inputs.
type Fake struct {
	// Response answers every call; Responses, when set, answers by call index.
	Response  string
	Responses []string
	Err       error
	Calls     []FakeCall
}

type FakeCall struct {
	Model string
	Input string
}

func (f *Fake) Run(ctx context.Context, model, input string) (string, error) {
	f.Calls = append(f.Calls, FakeCall{Model: model, Input: input})
	if f.Err != nil {
		return "", f.Err
	}
	if f.Responses != nil {
		if len(f.Calls) <= len(f.Responses) {
			return f.Responses[len(f.Calls)-1], nil
		}
		return f.Responses[len(f.Responses)-1], nil
	}
	return f.Response, nil
}
