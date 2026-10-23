// Package minisigntest makes minisign-format keys and signatures for tests,
// so tests don't need the minisign binary.
package minisigntest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// Signer holds a throwaway minisign key pair.
type Signer struct {
	KeyID [8]byte
	Priv  ed25519.PrivateKey
	Pub   ed25519.PublicKey
}

// New returns a fresh signer.
func New(t testing.TB) Signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := Signer{Priv: priv, Pub: pub}
	if _, err := rand.Read(s.KeyID[:]); err != nil {
		t.Fatal(err)
	}
	return s
}

// PublicKeyFile returns the key in minisign's .pub file format.
func (s Signer) PublicKeyFile() []byte {
	raw := append([]byte("Ed"), s.KeyID[:]...)
	raw = append(raw, s.Pub...)
	return []byte("untrusted comment: minisign public key\n" + base64.StdEncoding.EncodeToString(raw) + "\n")
}

// Sign returns a prehashed (ED) .minisig file for message.
func (s Signer) Sign(message []byte, trusted string) []byte {
	digest := blake2b.Sum512(message)
	sig := ed25519.Sign(s.Priv, digest[:])
	raw := append([]byte("ED"), s.KeyID[:]...)
	raw = append(raw, sig...)
	global := ed25519.Sign(s.Priv, append(sig, trusted...))
	return []byte(strings.Join([]string{
		"untrusted comment: signature from minisign secret key",
		base64.StdEncoding.EncodeToString(raw),
		"trusted comment: " + trusted,
		base64.StdEncoding.EncodeToString(global),
	}, "\n") + "\n")
}
