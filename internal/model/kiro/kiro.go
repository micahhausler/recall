// Package kiro runs prompts through Kiro CLI headless (`kiro-cli chat
// --no-interactive`), as a Runner; it has no Editor.
package kiro

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/model"
)

// New is a Runner with the default command and sessions directory.
func New() model.Runner { return &Runner{} }

// Runner runs Kiro CLI headless: `kiro-cli chat --no-interactive
// --output-format stream-json --trust-tools= --agent recall-headless
// --agent-engine v3 [--model <model>] [--effort <level>]` with the input
// on stdin, from a scratch dir whose agent has no tools. In the v3 engine
// --effort holds for that session only and outranks the owner's saved
// defaults.
// Kiro saves every run as a session, so the runner deletes it afterward;
// otherwise recall would read its own runs back as transcripts. The cleanup
// deletes only the session the stream named, the one session known to be
// this run's: other runners run beside this one (learn.map_workers), and
// the owner may open Kiro during a scheduled run, so a session that merely
// appeared meanwhile is never touched. A run that crashes before the stream
// names its session leaks it instead, with a log line; the map's self-input
// guard (learn.selfAuthored) keeps a leaked run from counting as the
// owner's.
//
// Runner is a model.Runner but not an Editor: a trusted Kiro tool is
// trusted on any path, outside the workspace too, so it can't keep apply's
// edits inside the worktrees. Apply runs it with no tools instead, and the
// binary writes the files its answer returns.
type Runner struct {
	// Command is the Kiro CLI binary, default config.DefaultCLI's.
	Command string
	// SessionsDir is where Kiro saves sessions, default ~/.kiro/sessions.
	SessionsDir string
	// Log is where a leaked session is reported, default os.Stderr.
	Log io.Writer
}

func (k *Runner) command() string {
	if k.Command == "" {
		return config.DefaultCLI(config.HarnessKiro)
	}
	return k.Command
}

// agentName names the agent every run uses, which Run writes into the
// run's scratch dir as agentConfig. The owner's default agent may allow
// tools without asking (reads under the working directory, or MCP
// servers), and --trust-tools= revokes none of them; this one has no
// tools, no MCP servers, and no resources. Managed settings can still add
// MCP servers to every agent, so it also denies every capability: a deny
// in any scope wins over an allow in any other.
const agentName = "recall-headless"

const agentConfig = `{
  "name": "recall-headless",
  "description": "recall's headless model calls: no tools",
  "tools": [],
  "allowedTools": [],
  "permissions": {"rules": [{"capability": "all", "effect": "deny"}]},
  "mcpServers": {},
  "includeMcpJson": false,
  "resources": []
}
`

// engine is pinned so a run doesn't change with the owner's
// chat.agentEngine setting; the stream parser reads both engines.
const engine = "v3"

// Check refuses an effort level kiro-cli doesn't take.
func (k *Runner) Check(call model.Call) error {
	return model.ValidEffort(config.HarnessKiro, call.Effort)
}

// Run sends input to call's model; an empty model or "auto" lets Kiro
// choose. It runs in an empty scratch dir, never the caller's (under recur,
// $HOME), holding only the agent file.
func (k *Runner) Run(ctx context.Context, call model.Call, input string) (string, model.Usage, error) {
	if err := k.Check(call); err != nil {
		return "", model.Usage{}, err
	}
	command := k.command()
	dir, cleanup, err := model.ScratchDir()
	if err != nil {
		return "", model.Usage{}, err
	}
	defer cleanup()
	agents := filepath.Join(dir, ".kiro", "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		return "", model.Usage{}, err
	}
	if err := os.WriteFile(filepath.Join(agents, agentName+".json"), []byte(agentConfig), 0o644); err != nil {
		return "", model.Usage{}, err
	}
	args := []string{"chat", "--no-interactive", "--output-format", "stream-json", "--trust-tools=",
		"--agent", agentName, "--agent-engine", engine}
	if call.Model != "" && call.Model != "auto" {
		args = append(args, "--model", call.Model)
	}
	if call.Effort != "" {
		args = append(args, "--effort", call.Effort)
	}
	var res *stream
	defer func() { k.deleteOwnSession(ctx, command, res) }()
	stdout, stderr, runErr := model.Cmd{Command: command, Args: args, Dir: dir, Input: input}.Run(ctx)
	res = parseStream(stdout)
	return res.result(command, stderr, runErr)
}

// sessionsDir is where Kiro saves sessions, ~/.kiro/sessions by default.
func (k *Runner) sessionsDir() string {
	if k.SessionsDir != "" {
		return k.SessionsDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return config.DefaultSource(config.HarnessKiro, home)
}

// listSessions lists the sessions on disk under dir as id to path, in both
// layouts Kiro writes: cli/<id>.jsonl, and <workspace hash>/sess_<id>/.
func listSessions(dir string) map[string]string {
	out := map[string]string{}
	if dir == "" {
		return out
	}
	top, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range top {
		if !e.IsDir() {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, s := range sub {
			name := s.Name()
			switch {
			case e.Name() == "cli" && !s.IsDir() && strings.HasSuffix(name, ".jsonl"):
				out[strings.TrimSuffix(name, ".jsonl")] = filepath.Join(dir, "cli", name)
			case s.IsDir() && strings.HasPrefix(name, "sess_"):
				out[name] = filepath.Join(dir, e.Name(), name)
			}
		}
	}
	return out
}

// deleteOwnSession deletes the one session this run is known to have
// created: the one the stream named, through the CLI first, then whatever
// files that left, all best-effort. Nothing else is ever deleted — a
// session that appeared in the directory during the run may be another
// runner's or one the owner opened, and taking someone else's session is
// worse than leaking recall's own. A run that ends before the stream names
// its session (an early crash) therefore deletes nothing and logs the leak;
// the map's self-input guard keeps the leaked run out of the loop.
func (k *Runner) deleteOwnSession(ctx context.Context, command string, res *stream) {
	// A fresh context: the run's may be what ended it.
	ctx = context.WithoutCancel(ctx)
	if res == nil || res.session == "" {
		fmt.Fprintf(k.log(), "recall: %s run ended without naming its session; one may be leaked under %s\n",
			command, k.sessionsDir())
		return
	}
	_ = exec.CommandContext(ctx, command, "chat", "--delete-session", res.session).Run()
	sessions := listSessions(k.sessionsDir())
	// The stream names a v3 session sess_<id> and a V2 one bare; look up
	// both spellings, so only this run's exact session can match.
	for _, id := range []string{res.session, "sess_" + res.session} {
		p, ok := sessions[id]
		if !ok {
			continue
		}
		_ = os.RemoveAll(p)
		// A V2 session's metadata sits beside its transcript.
		if strings.HasSuffix(p, ".jsonl") {
			_ = os.Remove(strings.TrimSuffix(p, ".jsonl") + ".json")
		}
	}
}

// log is where cleanup reports a leaked session.
func (k *Runner) log() io.Writer {
	if k.Log != nil {
		return k.Log
	}
	return os.Stderr
}

// stream is what a stream-json run reported.
type stream struct {
	session    string
	finished   bool
	status     string
	stopReason string
	finalText  string
	truncated  bool
	chunks     strings.Builder
	errMessage string
	credits    float64
}

type event struct {
	Type string `json:"type"`
	Data struct {
		SessionID          string `json:"sessionId"`
		Status             string `json:"status"`
		StopReason         string `json:"stopReason"`
		FinalText          string `json:"finalText"`
		FinalTextTruncated bool   `json:"finalTextTruncated"`
		Message            string `json:"message"`
		Update             struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Meta struct {
				Kiro struct {
					Kind    string `json:"kind"`
					Credits []struct {
						Usage float64 `json:"usage"`
					} `json:"credits"`
				} `json:"kiro"`
			} `json:"_meta"`
		} `json:"update"`
	} `json:"data"`
}

func parseStream(stdout []byte) *stream {
	res := &stream{}
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Data.SessionID != "" {
			res.session = ev.Data.SessionID
		}
		u := ev.Data.Update
		switch {
		case ev.Type == "runError":
			res.errMessage = ev.Data.Message
		case ev.Type == "runFinished":
			res.finished = true
			res.status, res.stopReason = ev.Data.Status, ev.Data.StopReason
			res.finalText, res.truncated = ev.Data.FinalText, ev.Data.FinalTextTruncated
		case u.SessionUpdate == "agent_message_chunk" && u.Content.Type == "text":
			res.chunks.WriteString(u.Content.Text)
		case u.Meta.Kiro.Kind == "session_usage":
			// Each report is the session's running total.
			res.credits = 0
			for _, c := range u.Meta.Kiro.Credits {
				res.credits += c.Usage
			}
		}
	}
	return res
}

func (r *stream) result(command, stderr string, runErr error) (string, model.Usage, error) {
	usage := model.Usage{Credits: r.credits}
	name := command + " chat"
	if r.stopReason == "refusal" {
		return "", usage, fmt.Errorf("%s: %w: %s", name, model.ErrRefusal, strings.TrimSpace(r.text()))
	}
	if r.errMessage != "" {
		return "", usage, fmt.Errorf("%s: %s", name, r.errMessage)
	}
	if !r.finished || r.status != "success" || runErr != nil {
		detail := r.status
		if r.stopReason != "" {
			detail += ": " + r.stopReason
		}
		if detail == "" {
			detail = stderr
		}
		if detail == "" && runErr != nil {
			detail = runErr.Error()
		}
		if detail == "" {
			detail = "no runFinished event"
		}
		return "", usage, fmt.Errorf("%s: %s", name, detail)
	}
	return r.text(), usage, nil
}

// text is the final answer: finalText, or the streamed chunks when Kiro
// truncated it.
func (r *stream) text() string {
	if r.truncated || r.finalText == "" {
		if s := r.chunks.String(); s != "" {
			return s
		}
	}
	return r.finalText
}
