// Package model runs prompts through a harness CLI in headless mode (for
// example `claude -p`), never an API client, so existing auth, billing, and
// model selection work unchanged.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Usage is what one call cost. The harness CLI reports it, so there's no
// pricing table to keep current here.
type Usage struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// Add accumulates another call's usage.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + o.InputTokens,
		OutputTokens: u.OutputTokens + o.OutputTokens,
		CostUSD:      u.CostUSD + o.CostUSD,
	}
}

func (u Usage) String() string {
	return fmt.Sprintf("%d in, %d out, $%.4f", u.InputTokens, u.OutputTokens, u.CostUSD)
}

// Runner answers one prompt with one completion.
type Runner interface {
	// Run sends input to the model named by model and returns its text and
	// what the call cost.
	Run(ctx context.Context, model, input string) (string, Usage, error)
}

// CLI runs a harness command headless: `<command> -p --model <model>` with
// the input on stdin. Every tool is disallowed, so the run reads stdin and
// writes stdout and nothing else. JSON output carries the result text plus
// the cost the harness reports.
type CLI struct {
	// Command is the harness binary, default "claude".
	Command string
}

// cliResult is claude -p --output-format json; other harnesses can register
// their own Runner when their shape differs.
type cliResult struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
}

func (c *CLI) Run(ctx context.Context, model, input string) (string, Usage, error) {
	command := c.Command
	if command == "" {
		command = "claude"
	}
	args := []string{"-p", "--model", model, "--disallowedTools", "*", "--output-format", "json"}
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
		return "", Usage{}, fmt.Errorf("%s -p: %s", command, detail)
	}
	var res cliResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		// A harness that doesn't speak this JSON still works; cost stays 0.
		return out.String(), Usage{}, nil
	}
	usage := Usage{
		// The harness reports cached prompt tokens separately; they're all input.
		InputTokens:  res.Usage.InputTokens + res.Usage.CacheCreationInputTokens + res.Usage.CacheReadInputTokens,
		OutputTokens: res.Usage.OutputTokens,
		CostUSD:      res.TotalCostUSD,
	}
	if res.IsError {
		return "", usage, fmt.Errorf("%s -p: %s: %s", command, res.Subtype, strings.TrimSpace(res.Result))
	}
	return res.Result, usage, nil
}

// Fake is a Runner for tests: canned responses, recorded inputs.
type Fake struct {
	// Response answers every call; Responses, when set, answers by call index.
	Response  string
	Responses []string
	// Usage is reported per call.
	Usage Usage
	Err   error
	Calls []FakeCall
}

type FakeCall struct {
	Model string
	Input string
}

func (f *Fake) Run(ctx context.Context, model, input string) (string, Usage, error) {
	f.Calls = append(f.Calls, FakeCall{Model: model, Input: input})
	if f.Err != nil {
		return "", Usage{}, f.Err
	}
	if f.Responses != nil {
		if len(f.Calls) <= len(f.Responses) {
			return f.Responses[len(f.Calls)-1], f.Usage, nil
		}
		return f.Responses[len(f.Responses)-1], f.Usage, nil
	}
	return f.Response, f.Usage, nil
}
