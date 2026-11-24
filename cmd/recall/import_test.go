package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	importsv1 "github.com/micahhausler/recall/api/imports/v1"
	"github.com/micahhausler/recall/internal/archive"
	"github.com/micahhausler/recall/internal/config"
	"github.com/micahhausler/recall/internal/imports"
)

const importConfig = "[harness.chat]\nformat = \"events\"\n\n[harness.chat.import]\ncommand = [\"chat-history\"]\n\n" +
	"[harness.notes]\nformat = \"events\"\n"

const importedSession = `{"session":"s-0001","cwd":"/home/you/src/example","timestamp":"2026-11-17T12:00:00Z","kind":"user_prompt","role":"user","text":"run the tests"}
{"session":"s-0001","cwd":"/home/you/src/example","timestamp":"2026-11-17T12:00:04Z","kind":"assistant_text","role":"assistant","text":"done"}
`

// fakeSources hands out one Fake per harness and records which ran.
type fakeSources struct {
	fakes map[string]*imports.Fake
	ran   []string
}

func (s *fakeSources) open(name string, imp *config.ImportSettings) imports.Source {
	s.ran = append(s.ran, name+":"+strings.Join(imp.Command, " "))
	return s.fakes[name]
}

func TestImportStoresAndReads(t *testing.T) {
	f := newHookFixtureWith(t, importConfig)
	src := &fakeSources{fakes: map[string]*imports.Fake{
		"chat": {Files: map[string]string{"one.jsonl": importedSession}, Status: importsv1.Status{Sessions: 1}},
	}}
	ctx := context.Background()
	out := captureStdout(t, func() {
		if code := runImports(ctx, f.env, nil, importFlags{}, src.open); code != 0 {
			t.Errorf("import = %d", code)
		}
	})
	if strings.Join(src.ran, ",") != "chat:chat-history" {
		t.Errorf("ran %v, want only the harness with an import table", src.ran)
	}
	if !strings.Contains(out, "import chat: asked for everything; 1 session(s) found, 1 stored, 0 unchanged") {
		t.Errorf("stdout = %q", out)
	}
	path := filepath.Join(config.ImportDir(f.home, "chat"), "sessions", "s-0001.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	var chat []string
	for _, h := range f.env.harnesses {
		if h.Name() != "chat" {
			continue
		}
		sessions, _ := h.Sessions()
		for _, s := range sessions {
			evs, err := h.ReadSessionFile(s, "laptop-a")
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range evs {
				chat = append(chat, e.Harness+"/"+e.Host+"/"+string(e.Kind))
			}
		}
	}
	if strings.Join(chat, ",") != "chat/laptop-a/user_prompt,chat/laptop-a/assistant_text" {
		t.Errorf("the chat harness reads %v", chat)
	}

	out = captureStdout(t, func() {
		if code := runImports(ctx, f.env, []string{"chat"}, importFlags{}, src.open); code != 0 {
			t.Errorf("second import = %d", code)
		}
	})
	if !strings.Contains(out, "asked for changes since ") || !strings.Contains(out, "0 stored, 1 unchanged") {
		t.Errorf("second run stdout = %q", out)
	}
}

func TestImportRefusesAHarnessWithoutAPlugin(t *testing.T) {
	f := newHookFixtureWith(t, importConfig)
	src := &fakeSources{fakes: map[string]*imports.Fake{}}
	errOut := captureStderr(t, func() {
		if code := runImports(context.Background(), f.env, []string{"notes", "missing"}, importFlags{}, src.open); code != 1 {
			t.Errorf("import = %d, want 1", code)
		}
	})
	if len(src.ran) != 0 {
		t.Errorf("ran %v", src.ran)
	}
	if !strings.Contains(errOut, `harness "notes" has no [harness.notes.import] table`) || !strings.Contains(errOut, `no enabled harness "missing"`) {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestImportFailureExitsNonZero(t *testing.T) {
	f := newHookFixtureWith(t, importConfig)
	src := &fakeSources{fakes: map[string]*imports.Fake{"chat": {Err: errors.New("log in again: the session expired")}}}
	errOut := captureStderr(t, func() {
		if code := runImports(context.Background(), f.env, nil, importFlags{}, src.open); code != 1 {
			t.Errorf("import = %d, want 1", code)
		}
	})
	if !strings.Contains(errOut, "recall: import chat: log in again: the session expired; nothing was stored") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestImportWarnsWhenAnotherHostImports(t *testing.T) {
	f := newHookFixtureWith(t, importConfig)
	ctx := context.Background()
	st, _, err := openStore(ctx, f.env)
	if err != nil {
		t.Fatal(err)
	}
	y, w := f.clk.Now().AddDate(0, 0, -7).ISOWeek()
	if err := st.PutIfAbsent(ctx, archive.Key("chat", "laptop-b", archive.Week{Year: y, Num: w}), bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	old := f.clk.Now().AddDate(0, 0, -7*20)
	oy, ow := old.ISOWeek()
	if err := st.PutIfAbsent(ctx, archive.Key("chat", "laptop-c", archive.Week{Year: oy, Num: ow}), bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	src := &fakeSources{fakes: map[string]*imports.Fake{"chat": {}}}
	var errOut string
	captureStdout(t, func() {
		errOut = captureStderr(t, func() { runImports(ctx, f.env, nil, importFlags{}, src.open) })
	})
	if !strings.Contains(errOut, "laptop-b archived this harness recently too") || strings.Contains(errOut, "laptop-c") {
		t.Errorf("stderr = %q", errOut)
	}
}
