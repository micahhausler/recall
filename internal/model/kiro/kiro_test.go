package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/model"
)

func TestStreamSuccess(t *testing.T) {
	out := strings.Join([]string{
		`{"type":"runStarted","data":{"payloadSchema":"acp","engine":"v3"}}`,
		`{"type":"sessionUpdate","data":{"sessionId":"sess_a","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"o"}}}}`,
		`{"type":"sessionUpdate","data":{"sessionId":"sess_a","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"k"}}}}`,
		`{"type":"sessionUpdate","data":{"sessionId":"sess_a","update":{"sessionUpdate":"session_info_update","_meta":{"kiro":{"kind":"session_usage","credits":[{"usage":0.05,"unit":"credit"}]}}}}}`,
		`{"type":"sessionUpdate","data":{"sessionId":"sess_a","update":{"sessionUpdate":"session_info_update","_meta":{"kiro":{"kind":"session_usage","credits":[{"usage":0.12,"unit":"credit"}]}}}}}`,
		`{"type":"runFinished","data":{"sessionId":"sess_a","status":"success","stopReason":"end_turn","finalText":"ok","finalTextTruncated":false}}`,
	}, "\n")
	res := parseStream([]byte(out))
	text, u, err := res.result("kiro-cli", "", nil)
	if err != nil || text != "ok" || u.Credits != 0.12 || res.session != "sess_a" {
		t.Errorf("got %q, %+v, %v, session %q", text, u, err, res.session)
	}
}

// A truncated finalText falls back to the streamed chunks.
func TestStreamTruncated(t *testing.T) {
	out := `{"type":"sessionUpdate","data":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"the whole answer"}}}}
{"type":"runFinished","data":{"sessionId":"s","status":"success","stopReason":"end_turn","finalText":"the wh","finalTextTruncated":true}}`
	text, _, err := parseStream([]byte(out)).result("kiro-cli", "", nil)
	if err != nil || text != "the whole answer" {
		t.Errorf("got %q, %v", text, err)
	}
}

func TestStreamRunError(t *testing.T) {
	out := `{"type":"runStarted","data":{}}
{"type":"runError","data":{"sessionId":"sess_b","stage":"prompt","message":"The model 'x' is not available."}}`
	res := parseStream([]byte(out))
	_, _, err := res.result("kiro-cli", "", errors.New("exit status 1"))
	if err == nil || err.Error() != "kiro-cli chat: The model 'x' is not available." || res.session != "sess_b" {
		t.Errorf("err = %v, session %q", err, res.session)
	}
}

func TestStreamRefusal(t *testing.T) {
	out := `{"type":"runFinished","data":{"sessionId":"s","status":"error","stopReason":"refusal","finalText":"declined"}}`
	_, _, err := parseStream([]byte(out)).result("kiro-cli", "", errors.New("exit status 1"))
	if !errors.Is(err, model.ErrRefusal) {
		t.Errorf("err = %v, want ErrRefusal", err)
	}
}

func TestStreamNoFinish(t *testing.T) {
	_, _, err := parseStream([]byte("garbage")).result("kiro-cli", "boom", errors.New("exit status 2"))
	if err == nil || err.Error() != "kiro-cli chat: boom" {
		t.Errorf("err = %v", err)
	}
}

// fakeCLI writes a shell script standing in for kiro-cli that logs its
// arguments to log and runs body for a chat run; a --delete-session call only
// logs and exits 0.
func fakeCLI(t *testing.T, body string) (script, log string) {
	t.Helper()
	bin := t.TempDir()
	script, log = filepath.Join(bin, "fake-kiro"), filepath.Join(bin, "log")
	text := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\ncase \"$*\" in *--delete-session*) exit 0;; esac\n%s", log, body)
	if err := os.WriteFile(script, []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, log
}

// A run that dies before the stream names a session deletes nothing: a
// session that appeared meanwhile may be another runner's or one the owner
// opened, so recall leaks its own rather than take someone else's, and logs
// the leak.
func TestRunEarlyFailureDeletesNothing(t *testing.T) {
	sessions := t.TempDir()
	old := filepath.Join(sessions, "0f0f", "sess_old")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "messages.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The crash saves a v3 and a V2 session but never prints a sessionId;
	// neither can be told apart from a concurrent run's or the owner's.
	script, log := fakeCLI(t, fmt.Sprintf(`mkdir -p %q/0f0f/sess_new %q/cli
echo '{}' > %q/0f0f/sess_new/messages.jsonl
echo '{}' > %q/cli/run1.jsonl
echo '{"cwd":"/work/a"}' > %q/cli/run1.json
exit 1
`, sessions, sessions, sessions, sessions, sessions))
	var leak strings.Builder
	k := &Runner{Command: script, SessionsDir: sessions, Log: &leak}
	if _, _, err := k.Run(context.Background(), model.Call{Model: "auto"}, "in"); err == nil {
		t.Fatal("Run succeeded, want an error")
	}
	for _, p := range []string{
		filepath.Join(old, "messages.jsonl"),
		filepath.Join(sessions, "0f0f", "sess_new", "messages.jsonl"),
		filepath.Join(sessions, "cli", "run1.jsonl"),
		filepath.Join(sessions, "cli", "run1.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("a session this run can't prove is its own was deleted: %v", err)
		}
	}
	if data, _ := os.ReadFile(log); strings.Contains(string(data), "--delete-session") {
		t.Errorf("CLI delete without a named session; log:\n%s", data)
	}
	if !strings.Contains(leak.String(), sessions) {
		t.Errorf("no leak log line naming %s; got %q", sessions, leak.String())
	}
}

// A successful run deletes exactly the session the stream named — the CLI
// record and its files — and never a session that appeared meanwhile, like
// one the owner opened during a scheduled run.
func TestRunDeletesNamedSession(t *testing.T) {
	sessions := t.TempDir()
	own := filepath.Join(sessions, "aaaa", "sess_a")
	owner := filepath.Join(sessions, "bbbb", "sess_owner")
	script, log := fakeCLI(t, fmt.Sprintf(`mkdir -p %q %q
echo '{}' > %q
echo '{}' > %q
printf '%%s\n' '{"type":"runFinished","data":{"sessionId":"sess_a","status":"success","stopReason":"end_turn","finalText":"ok"}}'
exit 0
`, own, owner, filepath.Join(own, "messages.jsonl"), filepath.Join(owner, "messages.jsonl")))
	k := &Runner{Command: script, SessionsDir: sessions}
	text, _, err := k.Run(context.Background(), model.Call{Model: "auto"}, "in")
	if err != nil || text != "ok" {
		t.Fatalf("Run = %q, %v", text, err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "--delete-session sess_a") {
		t.Errorf("no CLI delete of sess_a; log:\n%s", data)
	}
	if strings.Contains(string(data), "--delete-session sess_owner") {
		t.Errorf("CLI delete of the owner's session; log:\n%s", data)
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Errorf("own session left behind (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(owner, "messages.jsonl")); err != nil {
		t.Errorf("owner's session deleted: %v", err)
	}
}

// A V2 session the stream named is deleted with the metadata beside it.
func TestRunDeletesNamedV2Session(t *testing.T) {
	sessions := t.TempDir()
	script, _ := fakeCLI(t, fmt.Sprintf(`mkdir -p %q/cli
echo '{}' > %q/cli/run1.jsonl
echo '{"cwd":"/work/a"}' > %q/cli/run1.json
printf '%%s\n' '{"type":"runFinished","data":{"sessionId":"run1","status":"success","stopReason":"end_turn","finalText":"ok"}}'
exit 0
`, sessions, sessions, sessions))
	k := &Runner{Command: script, SessionsDir: sessions}
	if text, _, err := k.Run(context.Background(), model.Call{Model: "auto"}, "in"); err != nil || text != "ok" {
		t.Fatalf("Run = %q, %v", text, err)
	}
	for _, p := range []string{
		filepath.Join(sessions, "cli", "run1.jsonl"),
		filepath.Join(sessions, "cli", "run1.json"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind (stat err %v)", p, err)
		}
	}
}

// waitLoop is shell that waits for path to exist, giving up after ~10s.
func waitLoop(path string) string {
	return fmt.Sprintf(`i=0
while [ ! -e %q ]; do i=$((i+1)); [ "$i" -gt 400 ] && exit 9; sleep 0.025; done
`, path)
}

// Two overlapping runs (learn.map_workers runs several at once): the first
// to finish must not delete the second's in-flight session.
func TestRunConcurrentRunsDeleteOnlyOwn(t *testing.T) {
	sessions := t.TempDir()
	sessA := filepath.Join(sessions, "aaaa", "sess_a")
	sessB := filepath.Join(sessions, "bbbb", "sess_b")
	done := filepath.Join(t.TempDir(), "a_done")
	finish := func(id string) string {
		return fmt.Sprintf(`printf '%%s\n' '{"type":"runFinished","data":{"sessionId":%q,"status":"success","stopReason":"end_turn","finalText":"ok"}}'
exit 0
`, id)
	}
	// Run A saves its session, sees B's appear, and finishes while B is
	// still in flight.
	scriptA, logA := fakeCLI(t, fmt.Sprintf("mkdir -p %q\necho '{}' > %q\n", sessA, filepath.Join(sessA, "messages.jsonl"))+
		waitLoop(sessB)+finish("sess_a"))
	// Run B saves its session after A's and finishes only once the test has
	// checked what A's cleanup did.
	scriptB, logB := fakeCLI(t, waitLoop(sessA)+
		fmt.Sprintf("mkdir -p %q\necho '{}' > %q\n", sessB, filepath.Join(sessB, "messages.jsonl"))+
		waitLoop(done)+finish("sess_b"))
	kA := &Runner{Command: scriptA, SessionsDir: sessions}
	kB := &Runner{Command: scriptB, SessionsDir: sessions}
	errA, errB := make(chan error, 1), make(chan error, 1)
	go func() { _, _, err := kA.Run(context.Background(), model.Call{Model: "auto"}, "in"); errA <- err }()
	go func() { _, _, err := kB.Run(context.Background(), model.Call{Model: "auto"}, "in"); errB <- err }()
	if err := <-errA; err != nil {
		t.Fatalf("run A: %v", err)
	}
	// A finished and cleaned up; B is still running in its session.
	if _, err := os.Stat(filepath.Join(sessB, "messages.jsonl")); err != nil {
		t.Errorf("run A's cleanup took run B's in-flight session: %v", err)
	}
	if err := os.WriteFile(done, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-errB; err != nil {
		t.Fatalf("run B: %v", err)
	}
	for p, want := range map[string]string{logA: "sess_a", logB: "sess_b"} {
		data, _ := os.ReadFile(p)
		if !strings.Contains(string(data), "--delete-session "+want) {
			t.Errorf("no CLI delete of %s; log:\n%s", want, data)
		}
	}
	if data, _ := os.ReadFile(logA); strings.Contains(string(data), "--delete-session sess_b") {
		t.Errorf("run A deleted run B's session through the CLI; log:\n%s", data)
	}
	for _, p := range []string{sessA, sessB} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind (stat err %v)", p, err)
		}
	}
}

// Every run names recall's own agent and engine, and runs from a scratch dir
// holding that agent with no tools, so the owner's default agent and working
// directory never reach a headless run; the dir is gone afterward.
func TestRunUsesAToollessAgentInAScratchDir(t *testing.T) {
	sessions := t.TempDir()
	seen := filepath.Join(t.TempDir(), "seen")
	script, log := fakeCLI(t, fmt.Sprintf(`pwd > %q
cat .kiro/agents/recall-headless.json >> %q
printf '%%s\n' '{"type":"runFinished","data":{"sessionId":"sess_x","status":"success","stopReason":"end_turn","finalText":"ok"}}'
exit 0
`, seen, seen))
	k := &Runner{Command: script, SessionsDir: sessions}
	if text, _, err := k.Run(context.Background(), model.Call{Model: "auto"}, "in"); err != nil || text != "ok" {
		t.Fatalf("Run = %q, %v", text, err)
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), "--trust-tools= --agent recall-headless --agent-engine v3") {
		t.Errorf("args = %s", args)
	}
	data, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	dir, agent, _ := strings.Cut(string(data), "\n")
	if cwd, _ := os.Getwd(); dir == cwd {
		t.Errorf("ran in the caller's dir %s", dir)
	}
	var cfg struct {
		Tools      []string       `json:"tools"`
		MCPServers map[string]any `json:"mcpServers"`
		Resources  []string       `json:"resources"`
	}
	if err := json.Unmarshal([]byte(agent), &cfg); err != nil || len(cfg.Tools) != 0 || len(cfg.MCPServers) != 0 || len(cfg.Resources) != 0 {
		t.Errorf("agent = %s (%v)", agent, err)
	}
	if _, err := os.Stat(strings.TrimSpace(dir)); !os.IsNotExist(err) {
		t.Errorf("scratch dir %s left behind (stat err %v)", dir, err)
	}
}

// A set effort goes on the command line, with auto too; an unset one adds
// no flag, and a level kiro-cli doesn't take fails before Kiro runs.
func TestRunEffort(t *testing.T) {
	script, log := fakeCLI(t, `printf '{"type":"runFinished","data":{"sessionId":"s","status":"success","finalText":"%s"}}\n' "$*"
`)
	k := &Runner{Command: script, SessionsDir: t.TempDir()}
	for _, c := range []struct {
		call       model.Call
		want, deny string
	}{
		{model.Call{Model: "claude-sonnet-x", Effort: "high"}, "--model claude-sonnet-x --effort high", ""},
		{model.Call{Model: "gpt-x", Effort: "none"}, "--model gpt-x --effort none", ""},
		{model.Call{Model: "auto", Effort: "max"}, "--agent-engine v3 --effort max", "--model"},
		{model.Call{Model: "claude-sonnet-x"}, "--model claude-sonnet-x", "--effort"},
	} {
		got, _, err := k.Run(context.Background(), c.call, "in")
		if err != nil {
			t.Fatalf("%+v: %v", c.call, err)
		}
		if !strings.HasSuffix(got, c.want) || (c.deny != "" && strings.Contains(got, c.deny)) {
			t.Errorf("%+v args = %q, want them to end %q without %q", c.call, got, c.want, c.deny)
		}
	}
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.Run(context.Background(), model.Call{Model: "auto", Effort: "minimal"}, "in"); err == nil ||
		!strings.Contains(err.Error(), "minimal") {
		t.Errorf("effort minimal: err = %v", err)
	}
	if data, err := os.ReadFile(log); err == nil {
		t.Errorf("Kiro ran: %s", data)
	}
}

// Kiro takes its own CLI's effort levels.
func TestCheckEffort(t *testing.T) {
	for _, c := range []struct {
		effort string
		ok     bool
	}{
		{"none", true},
		{"max", true},
		{"minimal", false},
	} {
		err := model.Check(&Runner{}, model.Call{Model: "m", Effort: c.effort})
		if (err == nil) != c.ok {
			t.Errorf("Check(%q) = %v, want ok %v", c.effort, err, c.ok)
		}
	}
}
