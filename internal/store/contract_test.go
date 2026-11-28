package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// contract runs every Store through the same behavior checks.
func contract(t *testing.T, s Store) {
	t.Helper()
	contractUnder(t, s, "")
}

// contractUnder runs the checks with every key under prefix.
func contractUnder(t *testing.T, s Store, p string) {
	ctx := context.Background()
	t.Helper()

	if err := s.PutIfAbsent(ctx, p+"claude/laptop-a/2026/W45.tar.zst", strings.NewReader("one")); err != nil {
		t.Fatalf("first put: %v", err)
	}
	err := s.PutIfAbsent(ctx, p+"claude/laptop-a/2026/W45.tar.zst", strings.NewReader("two"))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second put: %v, want ErrExists", err)
	}

	r, err := s.Get(ctx, p+"claude/laptop-a/2026/W45.tar.zst")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	data, _ := io.ReadAll(r)
	r.Close()
	if string(data) != "one" {
		t.Errorf("get = %q, want the first write kept", data)
	}

	if _, err := s.Get(ctx, p+"claude/laptop-a/2026/W44.tar.zst"); !errors.Is(err, ErrNotExist) {
		t.Errorf("get missing: %v, want ErrNotExist", err)
	}

	info, err := s.Head(ctx, p+"claude/laptop-a/2026/W45.tar.zst")
	if err != nil || info.Size != 3 {
		t.Errorf("head = %+v, %v; want size 3", info, err)
	}
	if _, err := s.Head(ctx, p+"nope"); !errors.Is(err, ErrNotExist) {
		t.Errorf("head missing: %v, want ErrNotExist", err)
	}

	for _, key := range []string{p + "claude/laptop-b/2026/W45.tar.zst", p + "claude/laptop-a/2025/W52.tar.zst", p + "codex/laptop-a/2026/W45.tar.zst"} {
		if err := s.PutIfAbsent(ctx, key, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := s.List(ctx, p+"claude/laptop-a/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{p + "claude/laptop-a/2025/W52.tar.zst", p + "claude/laptop-a/2026/W45.tar.zst"}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] {
		t.Errorf("list = %v, want %v", keys, want)
	}
	all, err := s.List(ctx, p)
	if err != nil || len(all) != 4 {
		t.Errorf("list all = %v, %v; want 4 keys", all, err)
	}
	none, err := s.List(ctx, p+"kiro/")
	if err != nil || len(none) != 0 {
		t.Errorf("list none = %v, %v; want empty", none, err)
	}

	// Delete removes an object, and a missing key is no error, so the
	// reducer's host-review prune can repeat safely.
	if err := s.Delete(ctx, p+"claude/laptop-a/2025/W52.tar.zst"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, p+"claude/laptop-a/2025/W52.tar.zst"); !errors.Is(err, ErrNotExist) {
		t.Errorf("get after delete: %v, want ErrNotExist", err)
	}
	if err := s.Delete(ctx, p+"claude/laptop-a/2025/W52.tar.zst"); err != nil {
		t.Errorf("delete missing: %v, want nil", err)
	}

	// Put replaces: the reducer claim is the one object a handoff overwrites.
	if err := s.Put(ctx, p+"learn/reducer", strings.NewReader("laptop-a\n")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.Put(ctx, p+"learn/reducer", strings.NewReader("laptop-b\n")); err != nil {
		t.Fatalf("put over an existing object: %v", err)
	}
	r, err = s.Get(ctx, p+"learn/reducer")
	if err != nil {
		t.Fatalf("get after put: %v", err)
	}
	data, _ = io.ReadAll(r)
	r.Close()
	if string(data) != "laptop-b\n" {
		t.Errorf("get after put = %q, want the second write kept", data)
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

// A writer killed between claiming a key and filling it leaves an empty
// file. It is no object, and it must not block the key forever.
func TestFSReplacesADeadClaim(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	claim := filepath.Join(dir, "analysis", "a.json")
	if err := os.MkdirAll(filepath.Dir(claim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claim, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A claim just made belongs to a writer that may still be filling it.
	if err := st.PutIfAbsent(ctx, "analysis/a.json", strings.NewReader("{}")); !errors.Is(err, ErrExists) {
		t.Fatalf("a fresh claim was overwritten: %v", err)
	}
	old := time.Now().Add(-2 * claimGrace)
	if err := os.Chtimes(claim, old, old); err != nil {
		t.Fatal(err)
	}
	if err := st.PutIfAbsent(ctx, "analysis/a.json", strings.NewReader("{}")); err != nil {
		t.Fatalf("a dead claim blocked the key: %v", err)
	}
	if err := st.PutIfAbsent(ctx, "analysis/a.json", strings.NewReader("{}")); !errors.Is(err, ErrExists) {
		t.Errorf("a real object was overwritten: %v", err)
	}
}

// Two writers racing on a dead claim must produce exactly one winner, or the
// second rename silently overwrites the first and first-claim-wins is broken.
func TestFSDeadClaimTakeoverIsExclusive(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	claim := filepath.Join(dir, "a.json")
	if err := os.WriteFile(claim, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * claimGrace)
	if err := os.Chtimes(claim, old, old); err != nil {
		t.Fatal(err)
	}

	one, two := make(chan struct{}), make(chan struct{})
	results := make(chan error, 2)
	go func() {
		results <- st.PutIfAbsent(ctx, "a.json", &meetingReader{Reader: strings.NewReader("one"), here: one, there: two})
	}()
	go func() {
		results <- st.PutIfAbsent(ctx, "a.json", &meetingReader{Reader: strings.NewReader("two"), here: two, there: one})
	}()
	wins := 0
	for i := 0; i < 2; i++ {
		switch err := <-results; {
		case err == nil:
			wins++
		case !errors.Is(err, ErrExists):
			t.Fatalf("racing put: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d writers took over the dead claim, want exactly 1", wins)
	}
	got, err := os.ReadFile(claim)
	if err != nil {
		t.Fatalf("read after race: %v", err)
	}
	if string(got) != "one" && string(got) != "two" {
		t.Errorf("object after race = %q, want the winner's body", got)
	}
}

// meetingReader announces itself on its first Read and waits briefly for the
// other racer, so both writers sit past the claim check at the same time.
type meetingReader struct {
	io.Reader
	here, there chan struct{}
	met         bool
}

func (m *meetingReader) Read(p []byte) (int, error) {
	if !m.met {
		m.met = true
		close(m.here)
		select {
		case <-m.there:
		case <-time.After(100 * time.Millisecond):
		}
	}
	return m.Reader.Read(p)
}
