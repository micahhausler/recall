package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	eventsv1 "github.com/micahhausler/recall/api/events/v1"
	importsv1 "github.com/micahhausler/recall/api/imports/v1"
)

func request(t *testing.T, dir string, limits importsv1.Limits) string {
	t.Helper()
	b, err := json.Marshal(importsv1.Import{APIVersion: importsv1.APIVersion, Kind: importsv1.Kind,
		Spec: &importsv1.Spec{Name: "chat", OutputDir: dir, Since: "2026-03-01T05:00:00Z", Limits: limits}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func prompt(text string) eventsv1.Event {
	return eventsv1.Event{Timestamp: "2026-03-02T12:00:00Z", Kind: eventsv1.UserPrompt, Role: "user", Text: text}
}

func TestParseRequest(t *testing.T) {
	dir := t.TempDir()
	req, err := ParseRequest(request(t, dir, importsv1.Limits{Sessions: 5}))
	if err != nil {
		t.Fatal(err)
	}
	if req.Name != "chat" || req.OutputDir != dir || req.Since.Format("2006-01-02") != "2026-03-01" {
		t.Errorf("request = %+v", req)
	}
	if req.Limits.Sessions != 5 || req.Limits.LineBytes != importsv1.DefaultLimits.LineBytes {
		t.Errorf("limits = %+v, want the request's and defaults for the rest", req.Limits)
	}
	for name, info := range map[string]string{
		"empty":        "",
		"not json":     "{",
		"another kind": `{"apiVersion":"recall.credentials/v1","kind":"Credential","spec":{}}`,
		"no spec":      `{"apiVersion":"recall.import/v1","kind":"Import"}`,
		"relative dir": `{"apiVersion":"recall.import/v1","kind":"Import","spec":{"name":"chat","outputDir":"out"}}`,
		"bad since":    `{"apiVersion":"recall.import/v1","kind":"Import","spec":{"name":"chat","outputDir":"/o","since":"yesterday"}}`,
	} {
		if _, err := ParseRequest(info); err == nil {
			t.Errorf("ParseRequest accepts %s", name)
		}
	}
	if req, err := ParseRequest(`{"apiVersion":"recall.import/v1","kind":"Import","spec":{"name":"chat","outputDir":"/o","since":"","newer":1}}`); err != nil || !req.Since.IsZero() {
		t.Errorf("an empty since = %+v, %v", req, err)
	}
}

func TestRunWritesSessionsAndTheAnswer(t *testing.T) {
	dir := t.TempDir()
	var stdout bytes.Buffer
	err := Run(context.Background(), request(t, dir, importsv1.DefaultLimits), &stdout,
		func(ctx context.Context, req Request, out *Output) error {
			s, err := out.Session("s-0001")
			if err != nil {
				return err
			}
			p := prompt("run the tests")
			p.MessageID = "u-1"
			reply := eventsv1.Event{Timestamp: "2026-03-02T12:00:03Z", MessageID: "a-1", ParentID: "u-1", Kind: eventsv1.AssistantText, Role: "assistant", Text: "done"}
			for _, e := range []eventsv1.Event{p, reply} {
				if err := s.Add(e); err != nil {
					return err
				}
			}
			if err := s.Close(); err != nil {
				return err
			}
			out.Filtered(2)
			out.Partial()
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	var answer importsv1.Import
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil || answer.APIVersion != importsv1.APIVersion || answer.Kind != importsv1.Kind ||
		answer.Status == nil || *answer.Status != (importsv1.Status{Sessions: 1, Filtered: 2, Partial: true}) {
		t.Errorf("answer = %s, %v", stdout.String(), err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "s-0001.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"harness":"chat","session":"s-0001"`) || !strings.Contains(lines[1], `"parent_id":"u-1"`) {
		t.Errorf("session file = %s", data)
	}
	if st, _ := os.Stat(filepath.Join(dir, "s-0001.jsonl")); st.Mode().Perm() != 0o600 {
		t.Errorf("session file mode = %v", st.Mode().Perm())
	}
}

func TestSessionRefusals(t *testing.T) {
	dir := t.TempDir()
	run := func(limits importsv1.Limits, fetch func(*Output) error) error {
		return Run(context.Background(), request(t, dir, limits), &bytes.Buffer{},
			func(ctx context.Context, req Request, out *Output) error { return fetch(out) })
	}
	for _, id := range []string{"../x", "a/b", "a:b", "", ".x"} {
		if err := run(importsv1.DefaultLimits, func(out *Output) error { _, err := out.Session(id); return err }); err == nil {
			t.Errorf("Session(%q) accepted", id)
		}
	}
	err := run(importsv1.DefaultLimits, func(out *Output) error {
		s, _ := out.Session("dup")
		s.Add(prompt("x"))
		s.Close()
		_, err := out.Session("dup")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "already written") {
		t.Errorf("a repeated id = %v", err)
	}
	err = run(importsv1.Limits{Sessions: 1}, func(out *Output) error {
		s, _ := out.Session("one")
		s.Add(prompt("x"))
		s.Close()
		_, err := out.Session("two")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "at most 1") {
		t.Errorf("past the session limit = %v", err)
	}
	err = run(importsv1.DefaultLimits, func(out *Output) error {
		out.Session("open")
		_, err := out.Session("second")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "still open") {
		t.Errorf("a second open session = %v", err)
	}

	bad := map[string]eventsv1.Event{
		"a bad timestamp":    {Timestamp: "yesterday", Kind: eventsv1.UserPrompt},
		"an unknown kind":    {Timestamp: "2026-03-02T12:00:00Z", Kind: "hologram"},
		"ok on a call":       {Timestamp: "2026-03-02T12:00:00Z", Kind: eventsv1.ToolCall, OK: eventsv1.Bool(true)},
		"an orphan parent":   {Timestamp: "2026-03-02T12:00:00Z", Kind: eventsv1.AssistantText, MessageID: "a", ParentID: "missing"},
		"another session":    {Session: "other", Timestamp: "2026-03-02T12:00:00Z", Kind: eventsv1.UserPrompt},
		"a line over limits": {Timestamp: "2026-03-02T12:00:00Z", Kind: eventsv1.UserPrompt, Text: strings.Repeat("x", 200)},
	}
	for name, e := range bad {
		var addErr, closeErr error
		run(importsv1.Limits{LineBytes: 150}, func(out *Output) error {
			s, err := out.Session("bad")
			if err != nil {
				return err
			}
			addErr = s.Add(e)
			closeErr = s.Close()
			return nil
		})
		if addErr == nil || closeErr == nil {
			t.Errorf("%s: Add = %v, Close = %v; want both refused", name, addErr, closeErr)
		}
		if _, err := os.Stat(filepath.Join(dir, "bad.jsonl")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: a refused session's file was left behind", name)
		}
	}
	err = run(importsv1.Limits{SessionBytes: 300}, func(out *Output) error {
		s, _ := out.Session("big")
		for i := range 10 {
			if err := s.Add(prompt(fmt.Sprint(i))); err != nil {
				s.Close()
				return err
			}
		}
		return s.Close()
	})
	if err == nil || !strings.Contains(err.Error(), "300 bytes") {
		t.Errorf("past the session size = %v", err)
	}
	var undated error
	run(importsv1.DefaultLimits, func(out *Output) error {
		s, _ := out.Session("undated")
		s.Add(eventsv1.Event{Kind: eventsv1.UserPrompt, Text: "x"})
		undated = s.Close()
		return nil
	})
	if undated == nil || !strings.Contains(undated.Error(), "timestamp") {
		t.Errorf("an undated session = %v", undated)
	}
}

// A plugin that fails prints no answer, so recall stores nothing.
func TestRunPrintsNoAnswerOnFailure(t *testing.T) {
	dir := t.TempDir()
	for name, fetch := range map[string]func(context.Context, Request, *Output) error{
		"an error": func(context.Context, Request, *Output) error { return errors.New("login expired") },
		"an open session": func(ctx context.Context, req Request, out *Output) error {
			s, _ := out.Session("left-open")
			return s.Add(prompt("x"))
		},
	} {
		var stdout bytes.Buffer
		if err := Run(context.Background(), request(t, dir, importsv1.DefaultLimits), &stdout, fetch); err == nil {
			t.Errorf("%s: Run succeeded", name)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: answer printed: %s", name, stdout.String())
		}
	}
	var stdout bytes.Buffer
	if err := Run(context.Background(), "", &stdout, nil); err == nil || stdout.Len() != 0 {
		t.Errorf("no request = %v, %q", err, stdout.String())
	}
}
