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
	got, _, err := (&CLI{Command: script}).Edit(context.Background(), "sonnet", "in", []string{a, b})
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
	if _, _, err := (&CLI{Command: script}).Edit(context.Background(), "sonnet", "in", nil); err == nil {
		t.Error("Edit with no dirs succeeded")
	}
}
