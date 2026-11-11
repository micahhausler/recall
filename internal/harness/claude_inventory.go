package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// Inventory is what a harness offers in a directory: its skills and the
// slash commands it accepts (skills and built-ins).
type Inventory struct {
	Skills   []string
	Commands []string
}

// claudeCLI is the Claude Code binary the inventory runs.
var claudeCLI = "claude"

// Inventory starts a headless session in dir and reads the skills and slash
// commands from its init event, the harness's own list, then stops it before
// the model answers. The session isn't saved, so it never shows up as a
// transcript.
func (c *Claude) Inventory(ctx context.Context, dir string) (Inventory, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, claudeCLI, "-p", "reply ok", "--model", "haiku",
		"--output-format", "stream-json", "--verbose", "--max-turns", "1",
		"--no-session-persistence", "--disallowedTools", "*")
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Inventory{}, err
	}
	if err := cmd.Start(); err != nil {
		return Inventory{}, fmt.Errorf("%s: %w", claudeCLI, err)
	}
	inv, perr := parseInit(stdout)
	cancel()
	_ = cmd.Wait() // killed on purpose once init is read
	if perr != nil {
		return Inventory{}, fmt.Errorf("%s -p init: %w", claudeCLI, perr)
	}
	return inv, nil
}

// parseInit reads stream-json lines until the system init event.
func parseInit(r io.Reader) (Inventory, error) {
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
		return Inventory{Skills: ev.Skills, Commands: append(ev.SlashCommands, ev.TerminalCommands...)}, nil
	}
	if err := sc.Err(); err != nil {
		return Inventory{}, err
	}
	return Inventory{}, errors.New("no init event")
}
