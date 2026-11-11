package store

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// contract runs every Store through the same behavior checks.
func contract(t *testing.T, s Store) {
	ctx := context.Background()
	t.Helper()

	if err := s.PutIfAbsent(ctx, "claude/laptop-a/2026/W45.tar.zst", strings.NewReader("one")); err != nil {
		t.Fatalf("first put: %v", err)
	}
	err := s.PutIfAbsent(ctx, "claude/laptop-a/2026/W45.tar.zst", strings.NewReader("two"))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second put: %v, want ErrExists", err)
	}

	r, err := s.Get(ctx, "claude/laptop-a/2026/W45.tar.zst")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	data, _ := io.ReadAll(r)
	r.Close()
	if string(data) != "one" {
		t.Errorf("get = %q, want the first write kept", data)
	}

	if _, err := s.Get(ctx, "claude/laptop-a/2026/W44.tar.zst"); !errors.Is(err, ErrNotExist) {
		t.Errorf("get missing: %v, want ErrNotExist", err)
	}

	info, err := s.Head(ctx, "claude/laptop-a/2026/W45.tar.zst")
	if err != nil || info.Size != 3 {
		t.Errorf("head = %+v, %v; want size 3", info, err)
	}
	if _, err := s.Head(ctx, "nope"); !errors.Is(err, ErrNotExist) {
		t.Errorf("head missing: %v, want ErrNotExist", err)
	}

	for _, key := range []string{"claude/laptop-b/2026/W45.tar.zst", "claude/laptop-a/2025/W52.tar.zst", "codex/laptop-a/2026/W45.tar.zst"} {
		if err := s.PutIfAbsent(ctx, key, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := s.List(ctx, "claude/laptop-a/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"claude/laptop-a/2025/W52.tar.zst", "claude/laptop-a/2026/W45.tar.zst"}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] {
		t.Errorf("list = %v, want %v", keys, want)
	}
	all, err := s.List(ctx, "")
	if err != nil || len(all) != 4 {
		t.Errorf("list all = %v, %v; want 4 keys", all, err)
	}
	none, err := s.List(ctx, "kiro/")
	if err != nil || len(none) != 0 {
		t.Errorf("list none = %v, %v; want empty", none, err)
	}
}

func TestMemContract(t *testing.T) {
	contract(t, NewMem())
}

func TestFSContract(t *testing.T) {
	fs, err := NewFS(t.TempDir() + "/store")
	if err != nil {
		t.Fatal(err)
	}
	contract(t, fs)
}

func TestFSRejectsEscapingKeys(t *testing.T) {
	fs, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{"../escape", "/abs", ""} {
		if err := fs.PutIfAbsent(ctx, key, strings.NewReader("x")); err == nil {
			t.Errorf("put %q succeeded, want an error", key)
		}
	}
}
