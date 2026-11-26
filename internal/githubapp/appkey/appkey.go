// Package appkey reads a GitHub App's RSA private key and names it the way
// GitHub does.
package appkey

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
)

// Parse reads an RSA private key from PEM, in the PKCS #1 form GitHub
// hands out ("RSA PRIVATE KEY") or PKCS #8 ("PRIVATE KEY").
func Parse(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("the key is %T; GitHub Apps sign with RSA", k)
		}
		return rk, nil
	}
	return nil, fmt.Errorf("PEM block is %q, not a private key", block.Type)
}

// Fingerprint is how GitHub's app settings page names a key: the SHA-256
// of its public key in DER (SubjectPublicKeyInfo), base64, after "SHA256:".
func Fingerprint(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return "SHA256:" + base64.StdEncoding.EncodeToString(sum[:]), nil
}
