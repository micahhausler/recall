// Package agenttest is an in-process SSH agent for tests.
package agenttest

import (
	"crypto/rsa"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh/agent"
)

// Fake is an in-process SSH agent on a unix socket, for tests.
type Fake struct {
	// Socket is where it listens.
	Socket string

	dir string
	ln  net.Listener
}

// NewFake serves an agent holding keys.
func NewFake(keys ...*rsa.PrivateKey) (*Fake, error) {
	ring := agent.NewKeyring()
	for _, k := range keys {
		if err := ring.Add(agent.AddedKey{PrivateKey: k}); err != nil {
			return nil, err
		}
	}
	return ServeFake(ring)
}

// ServeFake serves a on a new socket until Close. The socket goes under
// /tmp because a unix socket's path is capped near 100 bytes, which a
// test's own temporary directory can exceed.
func ServeFake(a agent.Agent) (*Fake, error) {
	dir, err := os.MkdirTemp("/tmp", "agent")
	if err != nil {
		return nil, err
	}
	f := &Fake{Socket: filepath.Join(dir, "s"), dir: dir}
	if f.ln, err = net.Listen("unix", f.Socket); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	go func() {
		for {
			conn, err := f.ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				agent.ServeAgent(a, conn)
			}()
		}
	}()
	return f, nil
}

// Close stops the agent and removes its socket.
func (f *Fake) Close() error {
	err := f.ln.Close()
	os.RemoveAll(f.dir)
	return err
}
