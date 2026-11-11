// Package model runs prompts through a harness CLI in headless mode (for
// example `claude -p`), never an API client, so existing auth, billing, and
// model selection work unchanged.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	StopReason   string  `json:"stop_reason"`
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
	// Unsaved: a saved run would land in the transcripts recall reads.
	args := []string{"-p", "--model", model, "--disallowedTools", "*", "--output-format", "json",
		"--no-session-persistence"}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = strings.NewReader(input)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	runErr := cmd.Run()
	return parseResult(command, out.Bytes(), strings.TrimSpace(errOut.String()), runErr)
}

// ErrRefusal is a run the model or its safeguards declined; the input, not
// the runner, is the problem, so a smaller input may still go through.
var ErrRefusal = errors.New("refusal")

// parseResult turns a run's output into the result text and usage. With
// --output-format json the harness reports a failed run, such as a refusal,
// in the JSON on stdout and exits non-zero with stderr empty, so stdout is
// parsed before the exit status is.
func parseResult(command string, stdout []byte, stderr string, runErr error) (string, Usage, error) {
	var res cliResult
	if err := json.Unmarshal(stdout, &res); err != nil || res.Type == "" {
		if runErr != nil {
			detail := stderr
			if detail == "" {
				detail = runErr.Error()
			}
			return "", Usage{}, fmt.Errorf("%s -p: %s", command, detail)
		}
		// A harness that doesn't speak this JSON still works; cost stays 0.
		return string(stdout), Usage{}, nil
	}
	usage := Usage{
		// The harness reports cached prompt tokens separately; they're all input.
		InputTokens:  res.Usage.InputTokens + res.Usage.CacheCreationInputTokens + res.Usage.CacheReadInputTokens,
		OutputTokens: res.Usage.OutputTokens,
		CostUSD:      res.TotalCostUSD,
	}
	if res.StopReason == "refusal" {
		return "", usage, fmt.Errorf("%s -p: %w: %s", command, ErrRefusal, strings.TrimSpace(res.Result))
	}
	if res.IsError || runErr != nil {
		detail := strings.TrimSpace(res.Result)
		if res.StopReason != "" {
			detail = res.StopReason + ": " + detail
		}
		if detail == "" {
			detail = res.Subtype
		}
		if detail == "" && runErr != nil {
			detail = runErr.Error()
		}
		return "", usage, fmt.Errorf("%s -p: %s", command, detail)
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
