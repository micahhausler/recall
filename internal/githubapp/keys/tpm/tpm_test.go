//go:build cgo

package tpm

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
	"github.com/google/go-tpm/tpm2/transport/simulator"

	"github.com/micahhausler/recall/internal/githubapp/app"
)

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// held keeps one simulated TPM open across a Key's signatures, each of
// which opens and closes the TPM: the simulator is one process-wide TPM
// that forgets everything when closed.
type held struct{ transport.TPM }

func (held) Close() error { return nil }

// sim starts a simulated TPM with a fresh random seed, so its storage root
// key is its own, like a real machine's.
func sim(t *testing.T) transport.TPMCloser {
	t.Helper()
	tpm, err := simulator.OpenSimulator()
	if err != nil {
		t.Fatalf("starting the TPM simulator: %v", err)
	}
	return tpm
}

// imported wraps the test key under tpm and opens the blob against it.
func imported(t *testing.T, tpm transport.TPM) (*Key, []byte) {
	t.Helper()
	blob, err := Import(tpm, testKey())
	if err != nil {
		t.Fatal(err)
	}
	k, err := Parse(blob)
	if err != nil {
		t.Fatal(err)
	}
	k.open = func() (transport.TPMCloser, error) { return held{tpm}, nil }
	return k, blob
}

// The acceptance test: import a key, sign a GitHub App JWT with it
// in the TPM, and verify the signature against the public key.
func TestImportThenSignAJWT(t *testing.T) {
	tpm := sim(t)
	defer tpm.Close()
	k, blob := imported(t, tpm)

	if pub, ok := k.Public().(*rsa.PublicKey); !ok || !pub.Equal(&testKey().PublicKey) {
		t.Fatalf("wrapped key's public half = %v, want the imported key's", k.Public())
	}
	// The blob holds no plaintext key material.
	for name, secret := range map[string][]byte{"p": testKey().Primes[0].Bytes(), "q": testKey().Primes[1].Bytes(), "d": testKey().D.Bytes()} {
		if strings.Contains(string(blob), string(secret)) {
			t.Errorf("the blob holds the key's %s in the clear", name)
		}
	}

	a := &app.App{ClientID: "Iv23liEXAMPLE", Key: k, Now: func() time.Time { return time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC) }}
	jwt, err := a.JWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT = %q", jwt)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey().PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("the TPM's signature doesn't verify against the public key: %v", err)
	}
	var claims struct{ Iss string }
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if json.Unmarshal(cb, &claims) != nil || claims.Iss != "Iv23liEXAMPLE" {
		t.Errorf("claims = %s", cb)
	}

	// Signing leaves nothing loaded: each signature flushes what it loaded.
	for range 5 {
		if _, err := a.JWT(); err != nil {
			t.Fatalf("signing again: %v", err)
		}
	}
	rsp, err := tpm2.GetCapability{Capability: tpm2.TPMCapHandles, Property: uint32(tpm2.TPMHTTransient) << 24, PropertyCount: 8}.Execute(tpm)
	if err != nil {
		t.Fatal(err)
	}
	if handles, err := rsp.CapabilityData.Data.Handles(); err != nil || len(handles.Handle) != 0 {
		t.Errorf("transient objects left in the TPM: %v, %v", handles, err)
	}
}

func TestSignRefusesOtherSchemes(t *testing.T) {
	tpm := sim(t)
	defer tpm.Close()
	k, _ := imported(t, tpm)
	digest := sha256.Sum256([]byte("x"))
	for name, opts := range map[string]crypto.SignerOpts{
		"PSS":     &rsa.PSSOptions{Hash: crypto.SHA256},
		"SHA-512": crypto.SHA512,
		"none":    nil,
	} {
		if _, err := k.Sign(nil, digest[:], opts); err == nil {
			t.Errorf("signed with %s", name)
		}
	}
	if _, err := k.Sign(nil, digest[:5], crypto.SHA256); err == nil {
		t.Error("signed a short digest")
	}
}

// A blob is bound to the TPM that wrapped it: another machine's TPM, with
// its own storage root key, refuses to load it.
func TestBlobLoadsOnlyInItsOwnTPM(t *testing.T) {
	first := sim(t)
	_, blob := imported(t, first)
	first.Close()

	other := sim(t)
	defer other.Close()
	k, err := Parse(blob)
	if err != nil {
		t.Fatal(err)
	}
	k.open = func() (transport.TPMCloser, error) { return held{other}, nil }
	digest := sha256.Sum256([]byte("x"))
	if _, err := k.Sign(nil, digest[:], crypto.SHA256); err == nil || !strings.Contains(err.Error(), "loading the key") {
		t.Fatalf("another TPM's signature err = %v, want the load refused", err)
	}
}

func TestParseRejectsWhatIsNotABlob(t *testing.T) {
	tpm := sim(t)
	defer tpm.Close()
	_, blob := imported(t, tpm)
	for name, data := range map[string][]byte{
		"empty":     nil,
		"text":      []byte("this is not a wrapped key\n"),
		"truncated": blob[:len(blob)-10],
		"trailing":  append(append([]byte{}, blob...), 0, 1, 2),
		"public only": func() []byte {
			n, _ := sized(blob)
			return blob[:n]
		}(),
	} {
		if _, err := Parse(data); err == nil {
			t.Errorf("%s parsed as a wrapped key", name)
		}
	}
}
