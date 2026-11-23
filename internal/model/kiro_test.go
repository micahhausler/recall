package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKiroStreamSuccess(t *testing.T) {
	out := strings.Join([]string{
		`{"type":"runStarted","data":{"payloadSchema":"acp","engine":"v3"}}`,
		`{"type":"sessionUpdate","data":{"sessionId":"sess_a","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"o"}}}}`,
		`{"type":"sessionUpdate","data":{"sessionId":"sess_a","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"k"}}}}`,
		`{"type":"sessionUpdate","data":{"sessionId":"sess_a","update":{"sessionUpdate":"session_info_update","_meta":{"kiro":{"kind":"session_usage","credits":[{"usage":0.05,"unit":"credit"}]}}}}}`,
		`{"type":"sessionUpdate","data":{"sessionId":"sess_a","update":{"sessionUpdate":"session_info_update","_meta":{"kiro":{"kind":"session_usage","credits":[{"usage":0.12,"unit":"credit"}]}}}}}`,
		`{"type":"runFinished","data":{"sessionId":"sess_a","status":"success","stopReason":"end_turn","finalText":"ok","finalTextTruncated":false}}`,
	}, "\n")
	res := parseKiroStream([]byte(out))
	text, u, err := res.result("kiro-cli", "", nil)
	if err != nil || text != "ok" || u.Credits != 0.12 || res.session != "sess_a" {
		t.Errorf("got %q, %+v, %v, session %q", text, u, err, res.session)
	}
}

// A truncated finalText falls back to the streamed chunks.
func TestKiroStreamTruncated(t *testing.T) {
	out := `{"type":"sessionUpdate","data":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"the whole answer"}}}}
{"type":"runFinished","data":{"sessionId":"s","status":"success","stopReason":"end_turn","finalText":"the wh","finalTextTruncated":true}}`
	text, _, err := parseKiroStream([]byte(out)).result("kiro-cli", "", nil)
	if err != nil || text != "the whole answer" {
		t.Errorf("got %q, %v", text, err)
	}
}

func TestKiroStreamRunError(t *testing.T) {
	out := `{"type":"runStarted","data":{}}
{"type":"runError","data":{"sessionId":"sess_b","stage":"prompt","message":"The model 'x' is not available."}}`
	res := parseKiroStream([]byte(out))
	_, _, err := res.result("kiro-cli", "", errors.New("exit status 1"))
	if err == nil || err.Error() != "kiro-cli chat: The model 'x' is not available." || res.session != "sess_b" {
		t.Errorf("err = %v, session %q", err, res.session)
	}
}

func TestKiroStreamRefusal(t *testing.T) {
	out := `{"type":"runFinished","data":{"sessionId":"s","status":"error","stopReason":"refusal","finalText":"declined"}}`
	_, _, err := parseKiroStream([]byte(out)).result("kiro-cli", "", errors.New("exit status 1"))
	if !errors.Is(err, ErrRefusal) {
		t.Errorf("err = %v, want ErrRefusal", err)
	}
}

func TestKiroStreamNoFinish(t *testing.T) {
	_, _, err := parseKiroStream([]byte("garbage")).result("kiro-cli", "boom", errors.New("exit status 2"))
	if err == nil || err.Error() != "kiro-cli chat: boom" {
		t.Errorf("err = %v", err)
	}
}

// fakeKiro writes a shell script standing in for kiro-cli that logs its
// arguments to log and runs body for a chat run; a --delete-session call only
// logs and exits 0.
func fakeKiro(t *testing.T, body string) (script, log string) {
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
func TestKiroRunEarlyFailureDeletesNothing(t *testing.T) {
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
	script, log := fakeKiro(t, fmt.Sprintf(`mkdir -p %q/0f0f/sess_new %q/cli
echo '{}' > %q/0f0f/sess_new/messages.jsonl
echo '{}' > %q/cli/run1.jsonl
echo '{"cwd":"/work/a"}' > %q/cli/run1.json
exit 1
`, sessions, sessions, sessions, sessions, sessions))
	var leak strings.Builder
	k := &Kiro{Command: script, SessionsDir: sessions, Log: &leak}
	if _, _, err := k.Run(context.Background(), "auto", "in"); err == nil {
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
func TestKiroRunDeletesNamedSession(t *testing.T) {
	sessions := t.TempDir()
	own := filepath.Join(sessions, "aaaa", "sess_a")
	owner := filepath.Join(sessions, "bbbb", "sess_owner")
	script, log := fakeKiro(t, fmt.Sprintf(`mkdir -p %q %q
echo '{}' > %q
echo '{}' > %q
printf '%%s\n' '{"type":"runFinished","data":{"sessionId":"sess_a","status":"success","stopReason":"end_turn","finalText":"ok"}}'
exit 0
`, own, owner, filepath.Join(own, "messages.jsonl"), filepath.Join(owner, "messages.jsonl")))
	k := &Kiro{Command: script, SessionsDir: sessions}
	text, _, err := k.Run(context.Background(), "auto", "in")
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
func TestKiroRunDeletesNamedV2Session(t *testing.T) {
	sessions := t.TempDir()
	script, _ := fakeKiro(t, fmt.Sprintf(`mkdir -p %q/cli
echo '{}' > %q/cli/run1.jsonl
echo '{"cwd":"/work/a"}' > %q/cli/run1.json
printf '%%s\n' '{"type":"runFinished","data":{"sessionId":"run1","status":"success","stopReason":"end_turn","finalText":"ok"}}'
exit 0
`, sessions, sessions, sessions))
	k := &Kiro{Command: script, SessionsDir: sessions}
	if text, _, err := k.Run(context.Background(), "auto", "in"); err != nil || text != "ok" {
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
func TestKiroRunConcurrentRunsDeleteOnlyOwn(t *testing.T) {
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
	scriptA, logA := fakeKiro(t, fmt.Sprintf("mkdir -p %q\necho '{}' > %q\n", sessA, filepath.Join(sessA, "messages.jsonl"))+
		waitLoop(sessB)+finish("sess_a"))
	// Run B saves its session after A's and finishes only once the test has
	// checked what A's cleanup did.
	scriptB, logB := fakeKiro(t, waitLoop(sessA)+
		fmt.Sprintf("mkdir -p %q\necho '{}' > %q\n", sessB, filepath.Join(sessB, "messages.jsonl"))+
		waitLoop(done)+finish("sess_b"))
	kA := &Kiro{Command: scriptA, SessionsDir: sessions}
	kB := &Kiro{Command: scriptB, SessionsDir: sessions}
	errA, errB := make(chan error, 1), make(chan error, 1)
	go func() { _, _, err := kA.Run(context.Background(), "auto", "in"); errA <- err }()
	go func() { _, _, err := kB.Run(context.Background(), "auto", "in"); errB <- err }()
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

func TestOpen(t *testing.T) {
	for _, name := range []string{"claude", "kiro"} {
		if _, err := Open(name); err != nil {
			t.Errorf("Open(%q): %v", name, err)
		}
	}
	if _, err := Open("nope"); err == nil || !strings.Contains(err.Error(), "claude, kiro") {
		t.Errorf("Open(nope) err = %v", err)
	}
}

// Kiro can't confine edits to directories, so it has no editor, and apply
// runs it with no tools instead of editing through another harness.
func TestOpenEditor(t *testing.T) {
	if _, err := OpenEditor("claude"); err != nil {
		t.Errorf("OpenEditor(claude): %v", err)
	}
	if _, err := OpenEditor("kiro"); !errors.Is(err, ErrNoEditor) {
		t.Errorf("OpenEditor(kiro) err = %v", err)
	}
	if _, err := OpenEditor("nope"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("OpenEditor(nope) err = %v", err)
	}
}

func TestUsageString(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 2, CostUSD: 0.01}
	if got := u.String(); strings.Contains(got, "credits") {
		t.Errorf("no-credit usage = %q", got)
	}
	u = u.Add(Usage{Credits: 0.5})
	if got := u.String(); !strings.Contains(got, "0.50 credits") {
		t.Errorf("credit usage = %q", got)
	}
	if u.Zero() || (Usage{}).Zero() != true {
		t.Errorf("Zero() wrong: %+v", u)
	}
}
