// Package keys resolves a key reference, <kind>:<path>, to where a machine
// holds the App's private key: "tpm" is a key wrapped by this machine's
// TPM, which signs without the key ever leaving it, "ssh-agent" is a key
// in the SSH agent listening at the path, which signs the same way, and
// "file" is a PEM file, as safe as the file is.
package keys

import (
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/micahhausler/recall/internal/githubapp/appkey"
	"github.com/micahhausler/recall/internal/githubapp/keys/keyfile"
	"github.com/micahhausler/recall/internal/githubapp/keys/sshagent"
	"github.com/micahhausler/recall/internal/githubapp/keys/tpm"
)

// Store is somewhere a new key can be put: a file, or a blob a TPM wraps.
// An SSH agent isn't one. Nothing here adds a key to an agent, which may
// hold keys only until it restarts; whoever runs it puts the key there.
type Store interface {
	// Ready reports why a key can't be stored, before anything
	// irreversible happens: a new App's key is handed over exactly once.
	Ready() error
	// Put stores key, never over an existing one.
	Put(key *rsa.PrivateKey) error
}

// kind is one place a key can live.
type kind struct {
	// open reads the key at path as a signer. fingerprint, when set, is
	// the key it must be.
	open func(path, fingerprint string) (crypto.Signer, error)
	// store is the Store at path; nil for a kind that only signs.
	store func(path string) Store
}

var kinds = map[string]kind{
	"file": {
		open:  func(path, _ string) (crypto.Signer, error) { return openFile(path) },
		store: func(path string) Store { return fileStore(path) },
	},
	"tpm": {
		open:  func(path, _ string) (crypto.Signer, error) { return tpm.Open(path) },
		store: func(path string) Store { return tpmStore(path) },
	},
	// The path is the agent's socket, and the key in it is named by
	// fingerprint.
	"ssh-agent": {
		open: func(path, fingerprint string) (crypto.Signer, error) { return sshagent.Open(path, fingerprint) },
	},
}

// Kinds lists the key kinds.
func Kinds() []string {
	names := make([]string, 0, len(kinds))
	for n := range kinds {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Ref is a parsed key reference.
type Ref struct {
	Kind, Path string
}

func (r Ref) String() string { return r.Kind + ":" + r.Path }

// Parse reads <kind>:<path>, expanding a leading ~/ against home.
func Parse(ref, home string) (Ref, error) {
	name, path, ok := strings.Cut(ref, ":")
	if !ok || name == "" || path == "" {
		return Ref{}, fmt.Errorf("key %q must be <kind>:<path>, with kind one of %s", ref, strings.Join(Kinds(), ", "))
	}
	if _, ok := kinds[name]; !ok {
		return Ref{}, fmt.Errorf("unknown key kind %q; want one of %s", name, strings.Join(Kinds(), ", "))
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		path = filepath.Join(home, rest)
	}
	// The config keeps the reference as typed, and a relative path would
	// name a different file from each directory the command runs in.
	if !filepath.IsAbs(path) {
		return Ref{}, fmt.Errorf("key %q: the path must be absolute, or start with ~/", ref)
	}
	return Ref{Kind: name, Path: path}, nil
}

// Open reads the key as a signer. A tpm key isn't touched until it signs.
// fingerprint is the key's fingerprint as GitHub lists it: an ssh-agent
// key is found by it, and for any kind a key with another fingerprint is
// an error. Empty skips the check.
func (r Ref) Open(fingerprint string) (crypto.Signer, error) {
	k, ok := kinds[r.Kind]
	if !ok {
		return nil, fmt.Errorf("unknown key kind %q", r.Kind)
	}
	signer, err := k.open(r.Path, fingerprint)
	if err != nil {
		return nil, err
	}
	if fingerprint != "" {
		got, err := appkey.Fingerprint(signer.Public())
		if err != nil {
			return nil, err
		}
		if got != fingerprint {
			return nil, fmt.Errorf("the key at %s is %s, not %s", r, got, fingerprint)
		}
	}
	return signer, nil
}

// Store is where a new key goes for this reference. It reports false for
// a kind that only signs with a key someone else put there: ssh-agent.
func (r Ref) Store() (Store, bool) {
	k, ok := kinds[r.Kind]
	if !ok || k.store == nil {
		return nil, false
	}
	return k.store(r.Path), true
}

func openFile(path string) (crypto.Signer, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %04o, so others on this machine can read the key; chmod 600 it", path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := appkey.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return key, nil
}

// fileStore keeps a key as a PEM file at the path.
type fileStore string

func (p fileStore) Ready() error { return keyfile.Ready(string(p)) }

func (p fileStore) Put(key *rsa.PrivateKey) error {
	return keyfile.Create(string(p), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// tpmStore keeps a key as a blob at the path, wrapped by this machine's TPM.
type tpmStore string

func (p tpmStore) Ready() error {
	if err := keyfile.Ready(string(p)); err != nil {
		return err
	}
	return tpm.Ready()
}

func (p tpmStore) Put(key *rsa.PrivateKey) error { return tpm.ImportFile(string(p), key) }
