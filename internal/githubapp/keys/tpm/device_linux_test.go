//go:build linux

package tpm

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

// TestRealDevice wraps a throwaway key with this machine's real TPM and
// signs with it. It runs only when RECALL_TPM_TEST=1, on a machine with a
// TPM the user may use; it leaves nothing in the TPM and writes only to a
// temp dir.
func TestRealDevice(t *testing.T) {
	if os.Getenv("RECALL_TPM_TEST") != "1" {
		t.Skip("set RECALL_TPM_TEST=1 to test against " + Device)
	}
	if err := Ready(); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sub", "throwaway.blob")
	if err := ImportFile(path, key); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("blob mode = %v, %v; want 0600", st.Mode(), err)
	}
	if err := ImportFile(path, key); err == nil {
		t.Error("ImportFile replaced an existing blob")
	}
	wrapped, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("signed in the real TPM"))
	for range 3 {
		sig, err := wrapped.Sign(nil, digest[:], crypto.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
			t.Fatalf("the TPM's signature doesn't verify: %v", err)
		}
	}
}
