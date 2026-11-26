package sshagent

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/micahhausler/recall/internal/githubapp/appkey"
	"github.com/micahhausler/recall/internal/githubapp/keys/sshagent/agenttest"
)

func rsaKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := appkey.Fingerprint(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return k, fp
}

func serve(t *testing.T, a agent.Agent) string {
	t.Helper()
	f, err := agenttest.ServeFake(a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f.Socket
}

// The agent's signature is the RS256 signature the key itself would make.
func TestSignsRS256WithTheKeyNamedByFingerprint(t *testing.T) {
	other, otherFP := rsaKey(t)
	mine, mineFP := rsaKey(t)
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ring := agent.NewKeyring()
	for _, k := range []any{&ed, other, mine} {
		if err := ring.Add(agent.AddedKey{PrivateKey: k}); err != nil {
			t.Fatal(err)
		}
	}
	socket := serve(t, ring)

	key, err := Open(socket, mineFP)
	if err != nil {
		t.Fatal(err)
	}
	if !mine.PublicKey.Equal(key.Public()) {
		t.Fatal("Open picked another key")
	}
	msg := []byte("header.claims")
	sig, err := crypto.SignMessage(key, rand.Reader, msg, crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(msg)
	want, err := rsa.SignPKCS1v15(nil, mine, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sig, want) {
		t.Error("the agent's signature isn't the key's PKCS #1 v1.5 SHA-256 signature")
	}
	if rsa.VerifyPKCS1v15(&other.PublicKey, crypto.SHA256, digest[:], sig) == nil {
		t.Error("the other key verifies the signature")
	}
	if k, err := Open(socket, otherFP); err != nil || !other.PublicKey.Equal(k.Public()) {
		t.Errorf("Open by the other fingerprint: %v", err)
	}

	// Only the one signature is offered, and no digest is taken.
	if _, err := key.Sign(rand.Reader, digest[:], crypto.SHA256); err == nil {
		t.Error("signed a digest")
	}
	if _, err := key.SignMessage(rand.Reader, msg, crypto.SHA512); err == nil {
		t.Error("signed with SHA-512")
	}
	if _, err := key.SignMessage(rand.Reader, msg, &rsa.PSSOptions{Hash: crypto.SHA256}); err == nil {
		t.Error("signed with PSS")
	}
}

func TestOpenNeedsTheKeyNamedAndPresent(t *testing.T) {
	held, heldFP := rsaKey(t)
	_, absentFP := rsaKey(t)
	f, err := agenttest.NewFake(held)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// The only RSA key in the agent still isn't picked without its name.
	if _, err := Open(f.Socket, ""); err == nil || !strings.Contains(err.Error(), "key_fingerprint") {
		t.Errorf("Open with no fingerprint = %v", err)
	}
	if _, err := Open(f.Socket, absentFP); err == nil || !strings.Contains(err.Error(), "lists 1 RSA key(s)") {
		t.Errorf("Open for a key the agent lacks = %v", err)
	}
	if k, err := Open(f.Socket, heldFP); err != nil || !held.PublicKey.Equal(k.Public()) {
		t.Errorf("Open for the held key = %v", err)
	}
	gone := f.Socket + "-gone"
	if _, err := Open(gone, heldFP); err == nil || !strings.Contains(err.Error(), gone) {
		t.Errorf("Open with no agent listening = %v", err)
	}
}

// sha1Agent ignores the request for SHA-256, as an agent older than
// rsa-sha2 does, and signs "ssh-rsa" with SHA-1.
type sha1Agent struct{ agent.ExtendedAgent }

func (a sha1Agent) SignWithFlags(key ssh.PublicKey, data []byte, _ agent.SignatureFlags) (*ssh.Signature, error) {
	return a.ExtendedAgent.Sign(key, data)
}

// refusingAgent stands for a person declining the agent's prompt.
type refusingAgent struct{ agent.ExtendedAgent }

func (refusingAgent) SignWithFlags(ssh.PublicKey, []byte, agent.SignatureFlags) (*ssh.Signature, error) {
	return nil, agent.ErrExtensionUnsupported
}

func TestABadAnswerIsAnErrorNotASignature(t *testing.T) {
	k, fp := rsaKey(t)
	for name, wrap := range map[string]func(agent.ExtendedAgent) agent.Agent{
		"signed with ssh-rsa": func(a agent.ExtendedAgent) agent.Agent { return sha1Agent{a} },
		"didn't sign":         func(a agent.ExtendedAgent) agent.Agent { return refusingAgent{a} },
	} {
		ring := agent.NewKeyring().(agent.ExtendedAgent)
		if err := ring.Add(agent.AddedKey{PrivateKey: k}); err != nil {
			t.Fatal(err)
		}
		key, err := Open(serve(t, wrap(ring)), fp)
		if err != nil {
			t.Fatal(err)
		}
		sig, err := key.SignMessage(rand.Reader, []byte("x"), crypto.SHA256)
		if err == nil || sig != nil || !strings.Contains(err.Error(), name) {
			t.Errorf("want an error holding %q, got %v", name, err)
		}
	}
}
