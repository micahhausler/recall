package sshagent

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The same signature from OpenSSH's own agent, holding a key in the PEM
// form GitHub hands out. It runs wherever ssh-agent and ssh-add are
// installed, against a throwaway agent and key.
func TestOpenSSHAgentSignsRS256(t *testing.T) {
	agentBin, err := exec.LookPath("ssh-agent")
	if err != nil {
		t.Skip("no ssh-agent on PATH")
	}
	addBin, err := exec.LookPath("ssh-add")
	if err != nil {
		t.Skip("no ssh-add on PATH")
	}
	dir, err := os.MkdirTemp("/tmp", "agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	env := []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH")}

	daemon := exec.Command(agentBin, "-D", "-a", socket)
	daemon.Env = env
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		daemon.Process.Kill()
		daemon.Wait()
	})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if conn, err := net.Dial("unix", socket); err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ssh-agent never listened")
		}
	}

	key, fp := rsaKey(t)
	keyFile := filepath.Join(dir, "app.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	add := exec.Command(addBin, keyFile)
	add.Env = append(env, "SSH_AUTH_SOCK="+socket)
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("ssh-add: %v\n%s", err, out)
	}

	held, err := Open(socket, fp)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("header.claims")
	sig, err := crypto.SignMessage(held, rand.Reader, msg, crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(msg)
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("OpenSSH's agent didn't make an RS256 signature: %v", err)
	}
}
