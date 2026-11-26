// Package sshagent signs with a key an SSH agent holds, such as
// 1Password's: the agent signs and the key never reaches this process.
//
// An agent asked for an "rsa-sha2-256" signature answers with PKCS #1 v1.5
// over SHA-256, which is a JWT's RS256, byte for byte.
package sshagent

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/micahhausler/recall/internal/githubapp/appkey"
)

// dialTimeout bounds connecting to the socket. The signature itself waits
// as long as the agent does, which may be on a person approving it.
const dialTimeout = 5 * time.Second

// Key is one RSA key in an agent.
type Key struct {
	socket string
	pub    *rsa.PublicKey
	ssh    ssh.PublicKey
}

func dial(socket string) (agent.ExtendedAgent, io.Closer, error) {
	conn, err := net.DialTimeout("unix", socket, dialTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("reaching the SSH agent at %s: %w", socket, err)
	}
	return agent.NewClient(conn), conn, nil
}

// list returns the agent's RSA keys by GitHub fingerprint.
func list(socket string) (map[string]*Key, error) {
	client, conn, err := dial(socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	held, err := client.List()
	if err != nil {
		return nil, fmt.Errorf("listing the keys in the SSH agent at %s: %w", socket, err)
	}
	found := map[string]*Key{}
	for _, k := range held {
		pub, err := ssh.ParsePublicKey(k.Blob)
		if err != nil {
			continue // a key type this build doesn't know isn't an RSA key
		}
		cp, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			continue
		}
		rk, ok := cp.CryptoPublicKey().(*rsa.PublicKey)
		if !ok {
			continue
		}
		fp, err := appkey.Fingerprint(rk)
		if err != nil {
			return nil, err
		}
		found[fp] = &Key{socket: socket, pub: rk, ssh: pub}
	}
	return found, nil
}

// Open finds the key with the fingerprint GitHub's settings page lists
// for it, among the keys the agent at socket holds. It is always chosen by
// fingerprint, never for being the only RSA key there. Listing asks the
// agent for nothing secret; the agent decides at signing time whether to
// ask a person.
func Open(socket, fingerprint string) (*Key, error) {
	if fingerprint == "" {
		return nil, errors.New("an SSH agent holds many keys; set key_fingerprint in the config to the fingerprint the App's settings page lists for this one")
	}
	found, err := list(socket)
	if err != nil {
		return nil, err
	}
	k, ok := found[fingerprint]
	if !ok {
		return nil, fmt.Errorf("the SSH agent at %s lists %d RSA key(s), none with the fingerprint %s; is the key in the agent, and is the agent unlocked?", socket, len(found), fingerprint)
	}
	return k, nil
}

// Public is the key's public half.
func (k *Key) Public() crypto.PublicKey { return k.pub }

// Sign always fails: an agent hashes the message itself and takes no
// digest. Callers use crypto.SignMessage, which reaches SignMessage.
func (k *Key) Sign(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
	return nil, errors.New("an SSH agent signs messages, not digests; sign with crypto.SignMessage")
}

// SignMessage has the agent sign msg with PKCS #1 v1.5 over SHA-256, the
// only signature this package asks for. The answer is checked against the
// public key before it is returned, so an agent that ignored the request
// for SHA-256 is an error here and not a rejected JWT later.
func (k *Key) SignMessage(_ io.Reader, msg []byte, opts crypto.SignerOpts) ([]byte, error) {
	if _, pss := opts.(*rsa.PSSOptions); pss || opts == nil || opts.HashFunc() != crypto.SHA256 {
		return nil, errors.New("an SSH agent key signs PKCS #1 v1.5 over SHA-256 only")
	}
	client, conn, err := dial(k.socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	sig, err := client.SignWithFlags(k.ssh, msg, agent.SignatureFlagRsaSha256)
	if err != nil {
		return nil, fmt.Errorf("the SSH agent at %s didn't sign (was the request approved?): %w", k.socket, err)
	}
	if sig.Format != ssh.KeyAlgoRSASHA256 {
		return nil, fmt.Errorf("the SSH agent at %s signed with %s, not %s", k.socket, sig.Format, ssh.KeyAlgoRSASHA256)
	}
	// An agent may drop a signature's leading zero bytes; RS256 wants the
	// modulus's full width.
	out := sig.Blob
	if size := k.pub.Size(); len(out) < size {
		out = append(make([]byte, size-len(out)), out...)
	}
	digest := sha256.Sum256(msg)
	if err := rsa.VerifyPKCS1v15(k.pub, crypto.SHA256, digest[:], out); err != nil {
		return nil, fmt.Errorf("the SSH agent at %s returned a signature its own key doesn't verify: %w", k.socket, err)
	}
	return out, nil
}
