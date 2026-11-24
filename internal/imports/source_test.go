package imports

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	importsv1 "github.com/micahhausler/recall/api/imports/v1"
)

const helperEnv = "RECALL_IMPORTS_TEST_HELPER"

// TestHelperProcess is the plugin Exec's tests run: the test binary again,
// in the mode helperEnv names.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	os.Exit(helper(mode))
}

func helper(mode string) int {
	answer := func(status string) {
		fmt.Printf(`{"apiVersion":"recall.import/v1","kind":"Import","status":%s}`+"\n", status)
	}
	switch mode {
	case "ok":
		var req importsv1.Import
		if err := json.Unmarshal([]byte(os.Getenv(importsv1.InfoEnv)), &req); err != nil || req.Spec == nil {
			fmt.Fprintln(os.Stderr, "bad request:", err)
			return 1
		}
		wd, _ := os.Getwd()
		in, _ := io.ReadAll(os.Stdin)
		report := map[string]any{"wd": wd, "outputDir": req.Spec.OutputDir, "hook": os.Getenv("RECALL_HOOK"),
			"stdin": len(in), "args": os.Args[len(os.Args)-1], "name": req.Spec.Name}
		data, _ := json.Marshal(report)
		os.WriteFile(filepath.Join(req.Spec.OutputDir, "report.json"), data, 0o600)
		answer(`{"sessions":0,"filtered":1}`)
	case "stderr":
		fmt.Fprint(os.Stderr, "fetching page 1\nfetching page 2\nno newline")
		answer(`{"sessions":0}`)
	case "exit3":
		fmt.Fprintln(os.Stderr, "log in again: the session expired")
		answer(`{"sessions":0}`)
		return 3
	case "signal":
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
	case "sleep":
		fmt.Fprintln(os.Stderr, "starting a long fetch")
		time.Sleep(time.Minute)
	case "noanswer":
	case "notjson":
		fmt.Println("done!")
	case "wrongkind":
		fmt.Println(`{"apiVersion":"recall.credentials/v1","kind":"Credential","status":{"sessions":0}}`)
	case "nosessions":
		answer(`{"partial":true}`)
	case "partial":
		answer(`{"sessions":3,"partial":true,"newer":"ignored"}`)
	}
	return 0
}

func helperCommand(t *testing.T, mode string) []string {
	t.Helper()
	t.Setenv(helperEnv, mode)
	return []string{os.Args[0], "-test.run=^TestHelperProcess$", "--", "arg-from-config"}
}

func fetch(t *testing.T, e *Exec) (importsv1.Status, string, error) {
	t.Helper()
	dir := t.TempDir()
	status, err := e.Fetch(context.Background(), importsv1.Spec{Name: "chat", OutputDir: dir, Limits: importsv1.DefaultLimits})
	return status, dir, err
}

func TestExecRunsTheContract(t *testing.T) {
	status, dir, err := fetch(t, &Exec{Command: helperCommand(t, "ok"), Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if status != (importsv1.Status{Filtered: 1}) {
		t.Errorf("status = %+v", status)
	}
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	json.Unmarshal(data, &report)
	real, _ := filepath.EvalSymlinks(dir)
	wd, _ := filepath.EvalSymlinks(fmt.Sprint(report["wd"]))
	if wd != real || report["outputDir"] != dir || report["hook"] != "1" || report["stdin"] != 0.0 ||
		report["args"] != "arg-from-config" || report["name"] != "chat" {
		t.Errorf("the plugin saw %v", report)
	}
}

func TestExecStreamsStderr(t *testing.T) {
	var stderr bytes.Buffer
	if _, _, err := fetch(t, &Exec{Command: helperCommand(t, "stderr"), Stderr: &stderr, Prefix: "chat: "}); err != nil {
		t.Fatal(err)
	}
	if want := "chat: fetching page 1\nchat: fetching page 2\nchat: no newline\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestExecFailures(t *testing.T) {
	for mode, want := range map[string]string{
		"exit3":      "exit status 3\nlog in again: the session expired",
		"signal":     "signal: killed",
		"noanswer":   "isn't one JSON object",
		"notjson":    "isn't one JSON object",
		"wrongkind":  `answered "recall.credentials/v1" "Credential"`,
		"nosessions": "no status.sessions",
	} {
		_, _, err := fetch(t, &Exec{Command: helperCommand(t, mode)})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", mode, err, want)
		}
	}
	status, _, err := fetch(t, &Exec{Command: helperCommand(t, "partial")})
	if err != nil || status != (importsv1.Status{Sessions: 3, Partial: true}) {
		t.Errorf("partial = %+v, %v", status, err)
	}
}

func TestExecTimesOut(t *testing.T) {
	began := time.Now()
	_, _, err := fetch(t, &Exec{Command: helperCommand(t, "sleep"), Timeout: 300 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "no answer after 300ms") || !strings.Contains(err.Error(), "starting a long fetch") {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(began); d > 10*time.Second {
		t.Errorf("the timeout took %s", d)
	}
}

// A bare name is looked up on PATH; a path runs that file.
func TestExecResolvesTheCommand(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "chat-history")
	body := fmt.Sprintf("#!/bin/sh\nexec %q -test.run='^TestHelperProcess$' -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(helperEnv, "partial")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, cmd := range [][]string{{"chat-history"}, {script}} {
		if status, _, err := fetch(t, &Exec{Command: cmd}); err != nil || status.Sessions != 3 {
			t.Errorf("%v = %+v, %v", cmd, status, err)
		}
	}
	if _, _, err := fetch(t, &Exec{Command: []string{"recall-no-such-plugin"}}); err == nil || !strings.Contains(err.Error(), "not found on PATH") {
		t.Errorf("a missing plugin = %v", err)
	}
}
