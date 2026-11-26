// Package claude runs prompts through Claude Code headless (`claude -p`),
// as a Runner and as the apply step's Editor.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/model"
)

// New is a Runner with the default command.
func New() model.Runner { return &Runner{} }

// NewEditor is an Editor with the default command.
func NewEditor() model.Editor { return &Runner{} }

// Runner runs a harness command headless: `<command> -p --model <model>
// [--effort <level>]` with the input on stdin. Every tool is disallowed, so the run reads stdin and
// writes stdout and nothing else. JSON output carries the result text plus
// the cost the harness reports.
type Runner struct {
	// Command is the harness binary, default Claude Code's.
	Command string
}

// cliResult is claude -p --output-format json.
type cliResult struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	StopReason   string  `json:"stop_reason"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	// ModelUsage is keyed by the full ID of each model the run called.
	ModelUsage map[string]struct {
		OutputTokens int `json:"outputTokens"`
	} `json:"modelUsage"`
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
}

// Check refuses an effort level claude doesn't take.
func (c *Runner) Check(call model.Call) error {
	return model.ValidEffort(config.HarnessClaude, call.Effort)
}

// callArgs are the model and effort flags for call.
func callArgs(call model.Call) []string {
	args := []string{"--model", call.Model}
	if call.Effort != "" {
		args = append(args, "--effort", call.Effort)
	}
	return args
}

func (c *Runner) Run(ctx context.Context, call model.Call, input string) (string, model.Usage, error) {
	if err := c.Check(call); err != nil {
		return "", model.Usage{}, err
	}
	// An empty scratch dir, not the caller's: the harness scans its working
	// directory at startup, and from $HOME that touches TCC-protected folders
	// (Photos, OneDrive) and prompts for access under recall's name. This run
	// has no tools, so it needs no files.
	dir, cleanup, err := model.ScratchDir()
	if err != nil {
		return "", model.Usage{}, err
	}
	defer cleanup()
	// Unsaved: a saved run would land in the transcripts recall reads.
	args := append([]string{"-p"}, callArgs(call)...)
	args = append(args, "--disallowedTools", "*", "--output-format", "json", "--no-session-persistence")
	return c.run(ctx, dir, call, input, args...)
}

// EditTools are the only tools an Edit run gets: no shell, no network.
const EditTools = "Read Edit Write Glob Grep"

// Edit runs the harness headless in dirs[0] with only EditTools, edits
// accepted without a prompt, and every other dir added. The harness accepts
// edits only inside those dirs, so a write anywhere else is refused.
func (c *Runner) Edit(ctx context.Context, call model.Call, input string, dirs []string) (string, model.Usage, error) {
	if len(dirs) == 0 {
		return "", model.Usage{}, errors.New("edit: no directories to edit in")
	}
	if err := c.Check(call); err != nil {
		return "", model.Usage{}, err
	}
	args := append([]string{"-p"}, callArgs(call)...)
	args = append(args, "--tools", EditTools, "--permission-mode", "acceptEdits",
		"--output-format", "json", "--no-session-persistence")
	for _, d := range dirs[1:] {
		args = append(args, "--add-dir", d)
	}
	return c.run(ctx, dirs[0], call, input, args...)
}

// EffortEnv is the environment variable that sets Claude Code's effort. It
// outranks --effort, so a run that sets effort leaves it out.
const EffortEnv = "CLAUDE_CODE_EFFORT_LEVEL"

func (c *Runner) run(ctx context.Context, dir string, call model.Call, input string, args ...string) (string, model.Usage, error) {
	command := c.Command
	if command == "" {
		command = config.DefaultCLI(config.HarnessClaude)
	}
	cmd := model.Cmd{Command: command, Args: args, Dir: dir, Input: input}
	if call.Effort != "" {
		cmd.Unset = []string{EffortEnv}
	}
	stdout, stderr, runErr := cmd.Run(ctx)
	return parseResult(command, stdout, stderr, runErr)
}

// parseResult turns a run's output into the result text and usage. With
// --output-format json the harness reports a failed run, such as a refusal,
// in the JSON on stdout and exits non-zero with stderr empty, so stdout is
// parsed before the exit status is.
func parseResult(command string, stdout []byte, stderr string, runErr error) (string, model.Usage, error) {
	var res cliResult
	if err := json.Unmarshal(stdout, &res); err != nil || res.Type == "" {
		if runErr != nil {
			detail := stderr
			if detail == "" {
				detail = runErr.Error()
			}
			return "", model.Usage{}, fmt.Errorf("%s -p: %s", command, detail)
		}
		// A harness that doesn't speak this JSON still works; cost stays 0.
		return string(stdout), model.Usage{}, nil
	}
	usage := model.Usage{
		// The harness reports cached prompt tokens separately; they're all input.
		InputTokens:  res.Usage.InputTokens + res.Usage.CacheCreationInputTokens + res.Usage.CacheReadInputTokens,
		OutputTokens: res.Usage.OutputTokens,
		CostUSD:      res.TotalCostUSD,
		ModelID:      res.modelID(),
	}
	if res.StopReason == "refusal" {
		return "", usage, fmt.Errorf("%s -p: %w: %s", command, model.ErrRefusal, strings.TrimSpace(res.Result))
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

// modelID is the model that answered: the one model the run called, or the
// one that wrote the most when it called several.
func (r cliResult) modelID() string {
	ids := slices.Sorted(maps.Keys(r.ModelUsage))
	best := ""
	for _, id := range ids {
		if best == "" || r.ModelUsage[id].OutputTokens > r.ModelUsage[best].OutputTokens {
			best = id
		}
	}
	return best
}
