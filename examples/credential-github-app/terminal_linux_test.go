package main

import (
	"os"
	"syscall"
	"testing"
)

// openTerminal returns a terminal of the test's own, on nobody's screen:
// the controlling end of a new pseudo-terminal, which on Linux is a
// terminal itself.
func openTerminal(t *testing.T) *os.File {
	t.Helper()
	control, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal to test with: %v", err)
	}
	t.Cleanup(func() { control.Close() })
	return control
}
