package harness

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
	"github.com/micahhausler/recall/internal/hook"
)

// kiroCLI is the Kiro CLI binary the inventory runs.
var kiroCLI = config.DefaultCLI(config.HarnessKiro)

// Inventory starts a headless Kiro session in dir and reads the skills and
// slash commands it advertises, the harness's own list, then stops it once
// the turn starts and deletes the session, so it never shows up as a
// transcript.
func (k *Kiro) Inventory(ctx context.Context, dir string) (Inventory, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, kiroCLI, "chat", "--no-interactive",
		"--output-format", "stream-json", "--trust-tools=", "reply ok")
	cmd.Dir = dir
	// The same guard model calls carry (model.HookGuard): this run is
	// recall's own, and recall's session hooks must leave it alone.
	cmd.Env = append(os.Environ(), hook.Guard)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Inventory{}, err
	}
	if err := cmd.Start(); err != nil {
		return Inventory{}, fmt.Errorf("%s: %w", kiroCLI, err)
	}
	inv, session, perr := parseKiroCommands(stdout)
	cancel()
	_ = cmd.Wait() // killed on purpose once the commands are read
	if session != "" {
		_ = exec.CommandContext(ctx, kiroCLI, "chat", "--delete-session", session).Run()
	}
	if perr != nil {
		return Inventory{}, fmt.Errorf("%s chat: %w", kiroCLI, perr)
	}
	return inv, nil
}

// kiroStreamLine is one stream-json line: an ACP session update, or the
// run's start, finish, or error.
type kiroStreamLine struct {
	Type string `json:"type"`
	Data struct {
		SessionID string `json:"sessionId"`
		Message   string `json:"message"`
		Update    struct {
			SessionUpdate     string `json:"sessionUpdate"`
			AvailableCommands []struct {
				Name string `json:"name"`
				Meta struct {
					Kiro struct {
						Type string `json:"type"`
					} `json:"kiro"`
				} `json:"_meta"`
			} `json:"availableCommands"`
			Meta struct {
				Kiro struct {
					Kind string `json:"kind"`
				} `json:"kiro"`
			} `json:"_meta"`
		} `json:"update"`
	} `json:"data"`
}

// parseKiroCommands reads stream-json until the turn starts, keeping the
// last command list Kiro advertised before it (MCP prompts arrive in later
// updates than skills and agents). It returns the session id too, so the
// caller can delete the session.
func parseKiroCommands(r io.Reader) (Inventory, string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	var inv Inventory
	var session string
	seen := false
	for sc.Scan() {
		var l kiroStreamLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.Data.SessionID != "" {
			session = l.Data.SessionID
		}
		switch {
		case l.Type == "runError":
			return Inventory{}, session, errors.New(l.Data.Message)
		case l.Type == "runFinished", l.Data.Update.Meta.Kiro.Kind == "turn_start":
			if !seen {
				return Inventory{}, session, errors.New("no available commands")
			}
			return inv, session, nil
		case l.Data.Update.SessionUpdate == "available_commands_update":
			seen = true
			inv = Inventory{}
			for _, c := range l.Data.Update.AvailableCommands {
				if c.Meta.Kiro.Type == "skill" {
					inv.Skills = append(inv.Skills, c.Name)
				}
				inv.Commands = append(inv.Commands, c.Name)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Inventory{}, session, err
	}
	if !seen {
		return Inventory{}, session, errors.New("no available commands")
	}
	return inv, session, nil
}
