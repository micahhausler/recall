package keys

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micahhausler/recall/internal/githubapp/appkey"
	"github.com/micahhausler/recall/internal/githubapp/keys/sshagent/agenttest"
)

func TestParse(t *testing.T) {
	r, err := Parse("tpm:~/.local/state/recall-example/app-key.blob", "/home/you")
	if err != nil || r.Kind != "tpm" || r.Path != "/home/you/.local/state/recall-example/app-key.blob" {
		t.Errorf("ref = %+v, %v", r, err)
	}
	if r, err := Parse("file:/etc/app.pem", "/home/you"); err != nil || r.String() != "file:/etc/app.pem" {
		t.Errorf("ref = %v, %v", r, err)
	}
	if r, err := Parse("ssh-agent:~/.1password/agent.sock", "/home/you"); err != nil || r.Kind != "ssh-agent" || r.Path != "/home/you/.1password/agent.sock" {
		t.Errorf("ref = %+v, %v", r, err)
	}
	for _, bad := range []string{"", "/just/a/path", "tpm:", ":x", "vault:x"} {
		if _, err := Parse(bad, "/home/you"); err == nil || !strings.Contains(err.Error(), "file") {
			t.Errorf("Parse(%q) err = %v, want the kinds listed", bad, err)
		}
	}
}

func TestFileKeyStoresOnceAndSigns(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ref := Ref{Kind: "file", Path: filepath.Join(t.TempDir(), "sub", "app.pem")}
	store, ok := ref.Store()
	if !ok {
		t.Fatal("a file reference has no store")
	}
	if err := store.Ready(); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(key); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(ref.Path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, %v; want 0600", st.Mode(), err)
	}
	fp, err := appkey.Fingerprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ref.Open(fp)
	if err != nil {
		t.Fatal(err)
	}
	// A key other than the one the config names is refused.
	if _, err := ref.Open("SHA256:someone-elses"); err == nil || !strings.Contains(err.Error(), fp) {
		t.Errorf("Open with another key's fingerprint = %v", err)
	}
	if _, err := ref.Open(""); err != nil {
		t.Errorf("Open without a fingerprint = %v", err)
	}
	digest := sha256.Sum256([]byte("x"))
	sig, err := signer.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil || rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig) != nil {
		t.Errorf("the stored key doesn't sign: %v", err)
	}
	// A stored key is never replaced: it may be the only copy.
	if err := store.Ready(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Ready over an existing key = %v", err)
	}
	if err := store.Put(key); err == nil {
		t.Error("Put replaced an existing key")
	}
	if _, err := (Ref{Kind: "file", Path: filepath.Join(t.TempDir(), "missing.pem")}).Open(""); err == nil {
		t.Error("opened a missing key")
	}
}

// An ssh-agent reference has no store: the agent must already hold the
// key, and signs with it.
func TestAgentKeyIsOpenedNotStored(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := agenttest.NewFake(key)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	ref := Ref{Kind: "ssh-agent", Path: agent.Socket}
	if _, ok := ref.Store(); ok {
		t.Error("an ssh-agent reference has a store")
	}
	for _, kind := range []string{"file", "tpm"} {
		if _, ok := (Ref{Kind: kind, Path: "/x"}).Store(); !ok {
			t.Errorf("a %s reference has no store", kind)
		}
	}
	fp, err := appkey.Fingerprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ref.Open(fp)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("x")
	sig, err := crypto.SignMessage(signer, rand.Reader, msg, crypto.SHA256)
	digest := sha256.Sum256(msg)
	if err != nil || rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig) != nil {
		t.Errorf("the agent's key doesn't sign: %v", err)
	}
	strangerFP, err := appkey.Fingerprint(&stranger.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ref.Open(strangerFP); err == nil {
		t.Error("opened a key the agent doesn't hold")
	}
	if _, err := ref.Open(""); err == nil {
		t.Error("opened an agent key without naming it")
	}
	if _, err := (Ref{Kind: "ssh-agent", Path: agent.Socket + "-gone"}).Open(fp); err == nil {
		t.Error("opened a key with no agent listening")
	}
}

// A reference built by hand with a kind nobody knows is an error, not a
// crash.
func TestUnknownKind(t *testing.T) {
	ref := Ref{Kind: "vault", Path: "/x"}
	if _, err := ref.Open(""); err == nil {
		t.Error("opened a key of an unknown kind")
	}
	if _, ok := ref.Store(); ok {
		t.Error("an unknown kind has a store")
	}
}
