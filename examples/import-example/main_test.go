package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	importsv1 "github.com/micahhausler/recall/api/imports/v1"
)

var update = flag.Bool("update", false, "rewrite testdata/expected from the source")

func request(t *testing.T, dir string) string {
	t.Helper()
	b, err := json.Marshal(importsv1.Import{APIVersion: importsv1.APIVersion, Kind: importsv1.Kind,
		Spec: &importsv1.Spec{Name: "example", OutputDir: dir, Limits: importsv1.DefaultLimits}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The source maps to the expected events, one file per session.
func TestMapsTheSource(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--source", filepath.Join("testdata", "source.json")}, request(t, dir), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var answer importsv1.Import
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil || answer.Status == nil ||
		*answer.Status != (importsv1.Status{Sessions: 2, Filtered: 1}) {
		t.Errorf("answer = %s, %v", stdout.String(), err)
	}
	got, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	var names []string
	for _, path := range got {
		name := filepath.Base(path)
		names = append(names, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join("testdata", "expected", name)
		if *update {
			os.MkdirAll(filepath.Dir(want), 0o755)
			if err := os.WriteFile(want, data, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		expected, err := os.ReadFile(want)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(expected) {
			t.Errorf("%s =\n%s\nwant\n%s", name, data, expected)
		}
	}
	if want := []string{"chat-c-0001.jsonl", "s-0001.jsonl"}; !reflect.DeepEqual(names, want) {
		t.Errorf("sessions = %v, want %v", names, want)
	}
}

func TestSourceErrorsPrintNoAnswer(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("{"), 0o644)
	for name, tc := range map[string]struct {
		args []string
		code int
		want string
	}{
		"no source":         {nil, 2, "Usage:"},
		"missing source":    {[]string{"--source", filepath.Join(dir, "missing.json")}, 1, "no such file"},
		"unreadable source": {[]string{"--source", bad}, 1, "bad.json"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), tc.args, request(t, dir), &stdout, &stderr); code != tc.code {
			t.Errorf("%s: exit %d, want %d", name, code, tc.code)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s: stdout %q, stderr %q", name, stdout.String(), stderr.String())
		}
	}
}
