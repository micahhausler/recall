package model

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
)

func init() {
	Register("kiro", func() Runner { return &Kiro{} })
}

// Kiro runs Kiro CLI headless: `kiro-cli chat --no-interactive
// --output-format stream-json --trust-tools= [--model <model>]` with the
// input on stdin. No tool is trusted, so a tool call is never approved.
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
// Kiro is a Runner but not an Editor: a trusted Kiro tool is trusted on any
// path, outside the workspace too, so it can't keep apply's edits inside
// the worktrees. Apply runs it with no tools instead, and the binary writes
// the files its answer returns.
type Kiro struct {
	// Command is the Kiro CLI binary, default "kiro-cli".
	Command string
	// SessionsDir is where Kiro saves sessions, default ~/.kiro/sessions.
	SessionsDir string
	// Log is where a leaked session is reported, default os.Stderr.
	Log io.Writer
}

func (k *Kiro) command() string {
	if k.Command == "" {
		return "kiro-cli"
	}
	return k.Command
}

// Run sends input to model; an empty model or "auto" lets Kiro choose.
func (k *Kiro) Run(ctx context.Context, model, input string) (string, Usage, error) {
	command := k.command()
	args := []string{"chat", "--no-interactive", "--output-format", "stream-json", "--trust-tools="}
	if model != "" && model != "auto" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = append(os.Environ(), HookGuard)
	cmd.Stdin = strings.NewReader(input)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	var res *kiroStream
	defer func() { k.deleteOwnSession(ctx, command, res) }()
	runErr := cmd.Run()
	res = parseKiroStream(out.Bytes())
	return res.result(command, strings.TrimSpace(errOut.String()), runErr)
}

// sessionsDir is where Kiro saves sessions, ~/.kiro/sessions by default.
func (k *Kiro) sessionsDir() string {
	if k.SessionsDir != "" {
		return k.SessionsDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kiro", "sessions")
}

// kiroSessions lists the sessions on disk under dir as id to path, in both
// layouts Kiro writes: cli/<id>.jsonl, and <workspace hash>/sess_<id>/.
func kiroSessions(dir string) map[string]string {
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
func (k *Kiro) deleteOwnSession(ctx context.Context, command string, res *kiroStream) {
	// A fresh context: the run's may be what ended it.
	ctx = context.WithoutCancel(ctx)
	if res == nil || res.session == "" {
		fmt.Fprintf(k.log(), "recall: %s run ended without naming its session; one may be leaked under %s\n",
			command, k.sessionsDir())
		return
	}
	_ = exec.CommandContext(ctx, command, "chat", "--delete-session", res.session).Run()
	sessions := kiroSessions(k.sessionsDir())
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
func (k *Kiro) log() io.Writer {
	if k.Log != nil {
		return k.Log
	}
	return os.Stderr
}

// kiroStream is what a stream-json run reported.
type kiroStream struct {
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

type kiroEvent struct {
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

func parseKiroStream(stdout []byte) *kiroStream {
	res := &kiroStream{}
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var ev kiroEvent
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

func (r *kiroStream) result(command, stderr string, runErr error) (string, Usage, error) {
	usage := Usage{Credits: r.credits}
	name := command + " chat"
	if r.stopReason == "refusal" {
		return "", usage, fmt.Errorf("%s: %w: %s", name, ErrRefusal, strings.TrimSpace(r.text()))
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
func (r *kiroStream) text() string {
	if r.truncated || r.finalText == "" {
		if s := r.chunks.String(); s != "" {
			return s
		}
	}
	return r.finalText
}
