package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/store"
)

func TestGUIPaths(t *testing.T) {
	linux := strings.Join(guiPaths("linux"), ":")
	if linux != "/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Errorf("linux = %s", linux)
	}
	darwin := strings.Join(guiPaths("darwin"), ":")
	for _, want := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin", "/opt/homebrew/bin", "/usr/local/bin"} {
		if !strings.Contains(darwin+":", want+":") {
			t.Errorf("darwin %s misses %s", darwin, want)
		}
	}
}

func TestOffGUIPath(t *testing.T) {
	dir := t.TempDir()
	onPath := filepath.Join(dir, "on")
	elsewhere := filepath.Join(dir, "elsewhere")
	for _, d := range []string{onPath, elsewhere} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{onPath}

	// A binary in a GUI path warns about nothing.
	if msg := offGUIPath(filepath.Join(onPath, "recall"), paths); msg != "" {
		t.Errorf("a binary on the GUI PATH warned: %s", msg)
	}

	// One anywhere else warns, naming its directory and the PATH.
	msg := offGUIPath(filepath.Join(elsewhere, "recall"), paths)
	for _, want := range []string{"warning", elsewhere, onPath} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning %q misses %q", msg, want)
		}
	}

	// A binary reachable through a link on a GUI path is fine: that's how
	// Homebrew installs land.
	real := filepath.Join(elsewhere, "recall")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(onPath, "recall")); err != nil {
		t.Fatal(err)
	}
	if msg := offGUIPath(real, paths); msg != "" {
		t.Errorf("a linked binary warned: %s", msg)
	}
}

// With two machines on one store, only one runs the reduce: the first init
// claims learn/reducer, and the second reads the claim and leaves
// hooks.learn_every off.
func TestOneReducerPerStore(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()

	// Machine a: nothing has claimed the store, so init turns the reduce on
	// and claims it.
	host, err := storeReducer(ctx, st)
	if err != nil || host != "" {
		t.Fatalf("fresh store reducer = %q, %v", host, err)
	}
	if err := claimReducer(ctx, st, "laptop-a"); err != nil {
		t.Fatal(err)
	}

	// Machine b: the claim holds, and a race for it changes nothing.
	host, err = storeReducer(ctx, st)
	if err != nil || host != "laptop-a" {
		t.Fatalf("claimed store reducer = %q, %v", host, err)
	}
	if err := claimReducer(ctx, st, "laptop-b"); err != nil {
		t.Fatalf("a second claim should be a no-op: %v", err)
	}
	if host, _ = storeReducer(ctx, st); host != "laptop-a" {
		t.Errorf("reducer = %q, want the first claim kept", host)
	}

	// Re-running init on the reducer is also a no-op.
	if err := claimReducer(ctx, st, "laptop-a"); err != nil {
		t.Fatal(err)
	}
}
