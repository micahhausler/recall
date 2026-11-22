//go:build !darwin && !linux

package command_test

import (
	"os"
	"testing"
)

// openTerminal skips the test: only Linux and macOS, where recall runs,
// have a pseudo-terminal these tests know how to open.
func openTerminal(t *testing.T) *os.File {
	t.Helper()
	t.Skip("no pseudo-terminal to test with on this system")
	return nil
}
