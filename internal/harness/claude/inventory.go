package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/harness"
	"github.com/micahhausler/recall/internal/hook"
)

// cli is the Claude Code binary the inventory runs.
var cli = config.DefaultCLI(config.HarnessClaude)

// Inventory starts a headless session in dir and reads the skills and slash
// commands from its init event, the harness's own list, then stops it before
// the model answers. The session isn't saved, so it never shows up as a
// transcript.
func (c *adapter) Inventory(ctx context.Context, dir string) (harness.Inventory, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, "-p", "reply ok", "--model", "haiku",
		"--output-format", "stream-json", "--verbose", "--max-turns", "1",
		"--no-session-persistence", "--disallowedTools", "*")
	cmd.Dir = dir
	// The same guard model calls carry (model.HookGuard): this run is
	// recall's own, and recall's session hooks must leave it alone.
	cmd.Env = append(os.Environ(), hook.Guard)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return harness.Inventory{}, err
	}
	if err := cmd.Start(); err != nil {
		return harness.Inventory{}, fmt.Errorf("%s: %w", cli, err)
	}
	inv, perr := parseInit(stdout)
	cancel()
	_ = cmd.Wait() // killed on purpose once init is read
	if perr != nil {
		return harness.Inventory{}, fmt.Errorf("%s -p init: %w", cli, perr)
	}
	return inv, nil
}

// parseInit reads stream-json lines until the system init event.
func parseInit(r io.Reader) (harness.Inventory, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var ev struct {
			Type             string   `json:"type"`
			Subtype          string   `json:"subtype"`
			Skills           []string `json:"skills"`
			SlashCommands    []string `json:"slash_commands"`
			TerminalCommands []string `json:"terminal_slash_commands"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Type != "system" || ev.Subtype != "init" {
			continue
		}
		return harness.Inventory{Skills: ev.Skills, Commands: append(ev.SlashCommands, ev.TerminalCommands...)}, nil
	}
	if err := sc.Err(); err != nil {
		return harness.Inventory{}, err
	}
	return harness.Inventory{}, errors.New("no init event")
}
