package command_test

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openTerminal returns the terminal end of a new pseudo-terminal: a
// terminal of the test's own, on nobody's screen.
func openTerminal(t *testing.T) *os.File {
	t.Helper()
	control, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal to test with: %v", err)
	}
	t.Cleanup(func() { control.Close() })
	// The terminal end has to be granted, unlocked, and named before it
	// opens; the controlling end alone isn't a terminal here.
	fd := control.Fd()
	var name [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCPTYGRANT, 0); errno != 0 {
		t.Fatalf("granting the pseudo-terminal: %v", errno)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCPTYUNLK, 0); errno != 0 {
		t.Fatalf("unlocking the pseudo-terminal: %v", errno)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name))); errno != 0 {
		t.Fatalf("naming the pseudo-terminal: %v", errno)
	}
	terminal, err := os.OpenFile(string(name[:bytes.IndexByte(name[:], 0)]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("opening the pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { terminal.Close() })
	return terminal
}
