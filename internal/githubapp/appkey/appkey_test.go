package appkey

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"sync"
	"testing"
)

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

func TestParseBothEncodings(t *testing.T) {
	key := testKey()
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	der8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der8})
	for name, data := range map[string][]byte{"PKCS #1": pkcs1, "PKCS #8": pkcs8} {
		got, err := Parse(data)
		if err != nil || !got.Equal(key) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, data := range map[string][]byte{
		"empty":       nil,
		"certificate": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}),
	} {
		if _, err := Parse(data); err == nil {
			t.Errorf("%s parsed as a key", name)
		}
	}
}

func TestFingerprint(t *testing.T) {
	fp, err := Fingerprint(&testKey().PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&testKey().PublicKey)
	sum := sha256.Sum256(der)
	if want := "SHA256:" + base64.StdEncoding.EncodeToString(sum[:]); fp != want {
		t.Errorf("fingerprint = %s, want %s", fp, want)
	}
}
