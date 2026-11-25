package model

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseResultSuccess(t *testing.T) {
	out := `{"type":"result","subtype":"success","is_error":false,"result":"hi","total_cost_usd":0.01,"usage":{"input_tokens":3,"cache_read_input_tokens":7,"output_tokens":2}}`
	text, u, err := parseResult("claude", []byte(out), "", nil)
	if err != nil || text != "hi" || u.InputTokens != 10 || u.OutputTokens != 2 || u.CostUSD != 0.01 {
		t.Errorf("got %q, %+v, %v", text, u, err)
	}
}

// A refusal exits non-zero with stderr empty; the reason is only in stdout.
func TestParseResultRefusalOnNonZeroExit(t *testing.T) {
	out := `{"type":"result","subtype":"success","is_error":true,"stop_reason":"refusal","result":"API Error: flagged","total_cost_usd":0.002}`
	_, u, err := parseResult("claude", []byte(out), "", errors.New("exit status 1"))
	if !errors.Is(err, ErrRefusal) || !strings.Contains(err.Error(), "refusal: API Error: flagged") {
		t.Errorf("err = %v, want ErrRefusal with the reason", err)
	}
	if u.CostUSD != 0.002 {
		t.Errorf("usage = %+v, want the reported cost kept", u)
	}
}

func TestParseResultNonJSONFailureUsesStderr(t *testing.T) {
	_, _, err := parseResult("other", []byte("garbage"), "boom", errors.New("exit status 2"))
	if err == nil || err.Error() != "other -p: boom" {
		t.Errorf("err = %v", err)
	}
}

func TestParseResultNonJSONSuccess(t *testing.T) {
	text, _, err := parseResult("other", []byte("plain"), "", nil)
	if err != nil || text != "plain" {
		t.Errorf("got %q, %v", text, err)
	}
}

// Edit runs in the first dir, adds the rest, and gets only the file tools.
func TestCLIEditArgs(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "fake-harness")
	// Echo the working directory and args back as the result text.
	body := "#!/bin/sh\nprintf '{\"type\":\"result\",\"result\":\"%s|%s\"}' \"$(pwd -P)\" \"$*\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	a, b := t.TempDir(), t.TempDir()
	got, _, err := (&CLI{Command: script}).Edit(context.Background(), Call{Model: "sonnet"}, "in", []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	dir, args, _ := strings.Cut(got, "|")
	if want, _ := filepath.EvalSymlinks(a); dir != want {
		t.Errorf("ran in %s, want %s", dir, want)
	}
	for _, want := range []string{"--tools " + EditTools, "--permission-mode acceptEdits", "--add-dir " + b,
		"--no-session-persistence", "--model sonnet"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q miss %q", args, want)
		}
	}
	if strings.Contains(args, "--add-dir "+a) {
		t.Errorf("args %q add the working dir again", args)
	}
	if _, _, err := (&CLI{Command: script}).Edit(context.Background(), Call{Model: "sonnet"}, "in", nil); err == nil {
		t.Error("Edit with no dirs succeeded")
	}
}

func TestCLIRunInEmptyScratchDir(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "fake-harness")
	// Report the working directory and how many entries it holds.
	body := "#!/bin/sh\nprintf '{\"type\":\"result\",\"result\":\"%s|%s\"}' \"$(pwd -P)\" \"$(ls -A | wc -l | tr -d ' ')\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(bin) // a non-empty cwd the run must not inherit
	got, _, err := (&CLI{Command: script}).Run(context.Background(), Call{Model: "haiku"}, "in")
	if err != nil {
		t.Fatal(err)
	}
	dir, n, _ := strings.Cut(got, "|")
	if want, _ := filepath.EvalSymlinks(bin); dir == want {
		t.Errorf("ran in the caller's dir %s", dir)
	}
	if n != "0" {
		t.Errorf("scratch dir %s held %s entries, want 0", dir, n)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("scratch dir %s left behind (stat err %v)", dir, err)
	}
}

// fakeClaude writes a stub harness that echoes its args and the effort
// variable back as the result text.
func fakeClaude(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-harness")
	body := "#!/bin/sh\nprintf '{\"type\":\"result\",\"result\":\"%s|env=%s\"}' \"$*\" \"${" + EffortEnv + "-unset}\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// A set effort goes on the command line and the effort variable, which
// outranks the flag, is dropped; an empty one passes neither change.
func TestCLIEffort(t *testing.T) {
	t.Setenv(EffortEnv, "low")
	c := &CLI{Command: fakeClaude(t)}
	ctx := context.Background()
	runs := map[string]func(Call) (string, Usage, error){
		"Run":  func(call Call) (string, Usage, error) { return c.Run(ctx, call, "in") },
		"Edit": func(call Call) (string, Usage, error) { return c.Edit(ctx, call, "in", []string{t.TempDir()}) },
	}
	for name, run := range runs {
		got, _, err := run(Call{Model: "sonnet", Effort: "high"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(got, "--model sonnet --effort high ") || !strings.HasSuffix(got, "|env=unset") {
			t.Errorf("%s with effort = %q, want --effort high and %s unset", name, got, EffortEnv)
		}
		got, _, err = run(Call{Model: "sonnet"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(got, "--effort") || !strings.HasSuffix(got, "|env=low") {
			t.Errorf("%s without effort = %q, want no --effort and %s kept", name, got, EffortEnv)
		}
		if _, _, err := run(Call{Model: "sonnet", Effort: "extreme"}); err == nil || !strings.Contains(err.Error(), "extreme") {
			t.Errorf("%s with an unknown effort: err = %v", name, err)
		}
	}
}

func TestParseResultModelID(t *testing.T) {
	out := `{"type":"result","result":"hi","modelUsage":{"claude-haiku-x":{"outputTokens":3},"claude-sonnet-x":{"outputTokens":40}}}`
	_, u, err := parseResult("claude", []byte(out), "", nil)
	if err != nil || u.ModelID != "claude-sonnet-x" {
		t.Errorf("ModelID = %q, %v; want the model that wrote the most", u.ModelID, err)
	}
	_, u, _ = parseResult("claude", []byte(`{"type":"result","result":"hi"}`), "", nil)
	if u.ModelID != "" {
		t.Errorf("ModelID = %q without modelUsage", u.ModelID)
	}
}

// Each runner takes its own CLI's effort levels; a runner without a check
// of its own takes any.
func TestCheckEffort(t *testing.T) {
	for _, c := range []struct {
		r      any
		effort string
		ok     bool
	}{
		{&CLI{}, "", true},
		{&CLI{}, "xhigh", true},
		{&CLI{}, "max", true},
		{&CLI{}, "none", false},
		{&CLI{}, "High", false},
		{&Kiro{}, "none", true},
		{&Kiro{}, "max", true},
		{&Kiro{}, "minimal", false},
		{&Fake{}, "anything", true},
	} {
		err := Check(c.r, Call{Model: "m", Effort: c.effort})
		if (err == nil) != c.ok {
			t.Errorf("Check(%T, %q) = %v, want ok %v", c.r, c.effort, err, c.ok)
		}
	}
}
