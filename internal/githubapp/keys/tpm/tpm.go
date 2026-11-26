// Package tpm keeps an RSA signing key wrapped by this machine's TPM.
//
// Import hands the TPM a private key and gets back a blob only that TPM
// can load; the blob on disk is useless anywhere else, and the plaintext
// key can be thrown away. Open reads a blob as a crypto.Signer: each
// signature loads the key into the TPM, signs there, and unloads it, so
// the private key is never in this process's memory after the import.
//
// The key's parent is the TCG reference RSA storage root key in the owner
// hierarchy, made on demand from its template. The TPM derives the same
// parent from the same template every time, so nothing is provisioned and
// no persistent handle is used. A blob that tpm2-tools' tpm2_import wrote
// under that same parent loads too: the file is its public part followed
// by its private part.
package tpm

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"

	"github.com/micahhausler/recall/internal/githubapp/keys/keyfile"
)

// Key is an RSA private key wrapped by a TPM. It implements crypto.Signer
// for PKCS #1 v1.5 signatures over SHA-256, which is all a JWT's RS256
// needs.
type Key struct {
	public  tpm2.TPM2BPublic
	private tpm2.TPM2BPrivate
	rsa     *rsa.PublicKey
	// open reaches the TPM for one signature.
	open func() (transport.TPMCloser, error)
}

// Open reads a wrapped key from path. It doesn't touch the TPM; the first
// signature does.
func Open(path string) (*Key, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := Parse(blob)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

// Parse reads a wrapped key: a TPM2B_PUBLIC followed by a TPM2B_PRIVATE.
func Parse(blob []byte) (*Key, error) {
	pubLen, err := sized(blob)
	if err != nil {
		return nil, fmt.Errorf("not a TPM-wrapped key: public part: %w", err)
	}
	privLen, err := sized(blob[pubLen:])
	if err != nil {
		return nil, fmt.Errorf("not a TPM-wrapped key: private part: %w", err)
	}
	if pubLen+privLen != len(blob) {
		return nil, fmt.Errorf("not a TPM-wrapped key: %d bytes follow the private part", len(blob)-pubLen-privLen)
	}
	pub, err := tpm2.Unmarshal[tpm2.TPM2BPublic](blob[:pubLen])
	if err != nil {
		return nil, fmt.Errorf("not a TPM-wrapped key: public part: %w", err)
	}
	priv, err := tpm2.Unmarshal[tpm2.TPM2BPrivate](blob[pubLen:])
	if err != nil {
		return nil, fmt.Errorf("not a TPM-wrapped key: private part: %w", err)
	}
	contents, err := pub.Contents()
	if err != nil {
		return nil, fmt.Errorf("not a TPM-wrapped key: public part: %w", err)
	}
	if contents.Type != tpm2.TPMAlgRSA {
		return nil, errors.New("the wrapped key isn't RSA")
	}
	parms, err := contents.Parameters.RSADetail()
	if err != nil {
		return nil, err
	}
	modulus, err := contents.Unique.RSA()
	if err != nil {
		return nil, err
	}
	rsaPub, err := tpm2.RSAPub(parms, modulus)
	if err != nil {
		return nil, err
	}
	return &Key{public: *pub, private: *priv, rsa: rsaPub, open: openDevice}, nil
}

// sized is the length of the size-prefixed structure at the start of b.
func sized(b []byte) (int, error) {
	if len(b) < 2 {
		return 0, errors.New("truncated")
	}
	n := 2 + int(b[0])<<8 + int(b[1])
	if n == 2 || n > len(b) {
		return 0, errors.New("truncated")
	}
	return n, nil
}

// Public is the key's RSA public key.
func (k *Key) Public() crypto.PublicKey { return k.rsa }

// Sign signs a SHA-256 digest with PKCS #1 v1.5 inside the TPM. The random
// source isn't used: the scheme is deterministic.
func (k *Key) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	if _, pss := opts.(*rsa.PSSOptions); pss || opts == nil || opts.HashFunc() != crypto.SHA256 {
		return nil, errors.New("a TPM-wrapped key signs PKCS #1 v1.5 over SHA-256 only")
	}
	if len(digest) != crypto.SHA256.Size() {
		return nil, fmt.Errorf("digest is %d bytes, not a SHA-256 digest", len(digest))
	}
	tpm, err := k.open()
	if err != nil {
		return nil, err
	}
	defer tpm.Close()

	parent, err := storageRoot(tpm)
	if err != nil {
		return nil, err
	}
	defer flush(tpm, parent.ObjectHandle)
	loaded, err := tpm2.Load{
		ParentHandle: tpm2.AuthHandle{Handle: parent.ObjectHandle, Name: parent.Name, Auth: tpm2.PasswordAuth(nil)},
		InPublic:     k.public,
		InPrivate:    k.private,
	}.Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("loading the key into the TPM: %w (a wrapped key loads only in the TPM that wrapped it)", err)
	}
	defer flush(tpm, loaded.ObjectHandle)
	rsp, err := tpm2.Sign{
		KeyHandle: tpm2.AuthHandle{Handle: loaded.ObjectHandle, Name: loaded.Name, Auth: tpm2.PasswordAuth(nil)},
		Digest:    tpm2.TPM2BDigest{Buffer: digest},
		InScheme: tpm2.TPMTSigScheme{
			Scheme:  tpm2.TPMAlgRSASSA,
			Details: tpm2.NewTPMUSigScheme(tpm2.TPMAlgRSASSA, &tpm2.TPMSSchemeHash{HashAlg: tpm2.TPMAlgSHA256}),
		},
		// The digest was made outside the TPM; a key that isn't restricted
		// signs it without the TPM's proof of having hashed it.
		Validation: tpm2.TPMTTKHashCheck{Tag: tpm2.TPMSTHashCheck, Hierarchy: tpm2.TPMRHNull},
	}.Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("signing in the TPM: %w", err)
	}
	sig, err := rsp.Signature.Signature.RSASSA()
	if err != nil {
		return nil, err
	}
	return sig.Sig.Buffer, nil
}

// storageRoot makes the reference RSA storage root key in the owner
// hierarchy. The caller flushes it.
func storageRoot(tpm transport.TPM) (*tpm2.CreatePrimaryResponse, error) {
	rsp, err := tpm2.CreatePrimary{
		PrimaryHandle: tpm2.TPMRHOwner,
		InPublic:      tpm2.New2B(tpm2.RSASRKTemplate),
	}.Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("making the TPM's storage root key: %w", err)
	}
	return rsp, nil
}

func flush(tpm transport.TPM, h tpm2.TPMHandle) {
	_, _ = tpm2.FlushContext{FlushHandle: h}.Execute(tpm)
}

// Ready reports whether this machine's TPM can wrap a key: it is there,
// this user may use it, and it makes the storage root key. A caller about
// to receive a key it can get only once checks this first.
func Ready() error {
	tpm, err := openDevice()
	if err != nil {
		return err
	}
	defer tpm.Close()
	parent, err := storageRoot(tpm)
	if err != nil {
		return err
	}
	flush(tpm, parent.ObjectHandle)
	return nil
}

// Import wraps key under tpm's storage root key and returns the blob Parse
// reads. The key crosses to the TPM encrypted to the storage root key, so
// it isn't readable on the bus between them. The imported key signs and
// does nothing else, and has no password: the blob and the TPM together
// are the credential.
func Import(tpm transport.TPM, key *rsa.PrivateKey) ([]byte, error) {
	if len(key.Primes) != 2 {
		return nil, fmt.Errorf("the key has %d primes; a TPM takes two", len(key.Primes))
	}
	parent, err := storageRoot(tpm)
	if err != nil {
		return nil, err
	}
	defer flush(tpm, parent.ObjectHandle)

	exponent := uint32(key.E)
	if key.E == 65537 {
		exponent = 0 // how a TPM writes the default exponent
	}
	public := tpm2.TPMTPublic{
		Type:    tpm2.TPMAlgRSA,
		NameAlg: tpm2.TPMAlgSHA256,
		// Not fixed to this TPM or parent: a TPM imports only a key that
		// says it came from elsewhere. Nothing can take it back out,
		// because no policy allows duplicating it.
		ObjectAttributes: tpm2.TPMAObject{UserWithAuth: true, SignEncrypt: true},
		Parameters: tpm2.NewTPMUPublicParms(tpm2.TPMAlgRSA, &tpm2.TPMSRSAParms{
			Symmetric: tpm2.TPMTSymDefObject{Algorithm: tpm2.TPMAlgNull},
			Scheme: tpm2.TPMTRSAScheme{
				Scheme:  tpm2.TPMAlgRSASSA,
				Details: tpm2.NewTPMUAsymScheme(tpm2.TPMAlgRSASSA, &tpm2.TPMSSigSchemeRSASSA{HashAlg: tpm2.TPMAlgSHA256}),
			},
			KeyBits:  tpm2.TPMKeyBits(key.N.BitLen()),
			Exponent: exponent,
		}),
		Unique: tpm2.NewTPMUPublicID(tpm2.TPMAlgRSA, &tpm2.TPM2BPublicKeyRSA{Buffer: key.N.Bytes()}),
	}
	sensitive := tpm2.TPMTSensitive{
		SensitiveType: tpm2.TPMAlgRSA,
		Sensitive:     tpm2.NewTPMUSensitiveComposite(tpm2.TPMAlgRSA, &tpm2.TPM2BPrivateKeyRSA{Buffer: key.Primes[0].Bytes()}),
	}
	name, err := tpm2.ObjectName(&public)
	if err != nil {
		return nil, err
	}
	parentPublic, err := parent.OutPublic.Contents()
	if err != nil {
		return nil, err
	}
	wrapTo, err := tpm2.ImportEncapsulationKey(parentPublic)
	if err != nil {
		return nil, err
	}
	duplicate, seed, err := tpm2.CreateDuplicate(rand.Reader, wrapTo, name.Buffer, tpm2.Marshal(sensitive))
	if err != nil {
		return nil, fmt.Errorf("wrapping the key for the TPM: %w", err)
	}
	imported, err := tpm2.Import{
		ParentHandle: tpm2.AuthHandle{Handle: parent.ObjectHandle, Name: parent.Name, Auth: tpm2.PasswordAuth(nil)},
		ObjectPublic: tpm2.New2B(public),
		Duplicate:    tpm2.TPM2BPrivate{Buffer: duplicate},
		InSymSeed:    tpm2.TPM2BEncryptedSecret{Buffer: seed},
		Symmetric:    tpm2.TPMTSymDef{Algorithm: tpm2.TPMAlgNull},
	}.Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("importing the key into the TPM: %w", err)
	}
	return append(tpm2.Marshal(tpm2.New2B(public)), tpm2.Marshal(imported.OutPrivate)...), nil
}

// ImportFile wraps key under this machine's TPM and writes the blob to
// path, readable by its owner only. It then signs with the wrapped key
// once, so a blob that doesn't work is never left looking like one that
// does. An existing file isn't replaced: it may be the only copy of a key.
func ImportFile(path string, key *rsa.PrivateKey) error {
	tpm, err := openDevice()
	if err != nil {
		return err
	}
	blob, err := Import(tpm, key)
	tpm.Close()
	if err != nil {
		return err
	}
	wrapped, err := Parse(blob)
	if err != nil {
		return err
	}
	if err := wrapped.check(); err != nil {
		return fmt.Errorf("the imported key doesn't sign: %w", err)
	}
	return keyfile.Create(path, blob)
}

// check signs a fixed digest in the TPM and verifies it against the key's
// public half.
func (k *Key) check() error {
	digest := make([]byte, crypto.SHA256.Size())
	sig, err := k.Sign(nil, digest, crypto.SHA256)
	if err != nil {
		return err
	}
	return rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, digest, sig)
}
