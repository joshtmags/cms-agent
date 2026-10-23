package minisign

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/joshtmags/cms-agent/internal/minisign/minisigntest"
)

func mustVerify(t *testing.T, pubFile, message, sigFile []byte) error {
	t.Helper()
	pk, err := ParsePublicKey(pubFile)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := ParseSignature(sigFile)
	if err != nil {
		return err
	}
	return Verify(pk, message, sig)
}

func TestVerifyAcceptsValidSignature(t *testing.T) {
	s := minisigntest.New(t)
	msg := []byte(`{"version":"1.1.0"}`)
	if err := mustVerify(t, s.PublicKeyFile(), msg, s.Sign(msg, "cms-arena-agent 1.1.0")); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestVerifyRejectsChangedMessage(t *testing.T) {
	s := minisigntest.New(t)
	sig := s.Sign([]byte(`{"version":"1.1.0"}`), "c")
	if err := mustVerify(t, s.PublicKeyFile(), []byte(`{"version":"9.9.9"}`), sig); err == nil {
		t.Fatal("changed message accepted")
	}
}

func TestVerifyRejectsOtherKey(t *testing.T) {
	signer, other := minisigntest.New(t), minisigntest.New(t)
	msg := []byte("m")
	if err := mustVerify(t, other.PublicKeyFile(), msg, signer.Sign(msg, "c")); err == nil {
		t.Fatal("signature from another key accepted")
	}
}

func TestVerifyRejectsSameKeyIDDifferentKey(t *testing.T) {
	signer, other := minisigntest.New(t), minisigntest.New(t)
	other.KeyID = signer.KeyID
	msg := []byte("m")
	if err := mustVerify(t, other.PublicKeyFile(), msg, signer.Sign(msg, "c")); err == nil {
		t.Fatal("signature accepted by a different key that reuses the key id")
	}
}

func TestVerifyRejectsTamperedTrustedComment(t *testing.T) {
	s := minisigntest.New(t)
	msg := []byte("m")
	sig := strings.Replace(string(s.Sign(msg, "cms-arena-agent 1.1.0")), "1.1.0", "2.0.0", 1)
	if err := mustVerify(t, s.PublicKeyFile(), msg, []byte(sig)); err == nil {
		t.Fatal("tampered trusted comment accepted")
	}
}

func TestVerifyRejectsLegacyAlgorithm(t *testing.T) {
	s := minisigntest.New(t)
	msg := []byte("m")
	sig := ed25519.Sign(s.Priv, msg)
	raw := append([]byte("Ed"), s.KeyID[:]...)
	raw = append(raw, sig...)
	file := "untrusted comment: x\n" + base64.StdEncoding.EncodeToString(raw) +
		"\ntrusted comment: c\n" + base64.StdEncoding.EncodeToString(ed25519.Sign(s.Priv, append(sig, 'c'))) + "\n"
	if err := mustVerify(t, s.PublicKeyFile(), msg, []byte(file)); err == nil {
		t.Fatal("legacy Ed signature accepted")
	}
}

func TestParsePublicKeyRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "untrusted comment: x\n", "not base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := ParsePublicKey([]byte(in)); err == nil {
			t.Errorf("ParsePublicKey(%q) succeeded", in)
		}
	}
}
