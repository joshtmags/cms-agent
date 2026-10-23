// Package minisign verifies signatures made by the minisign tool
// (https://jedisct1.github.io/minisign/).
//
// Only the prehashed "ED" algorithm (minisign's default since 0.11) is
// accepted. Legacy "Ed" signatures are rejected on purpose: releases are
// always signed with a current minisign, so accepting both would only widen
// what the PCs trust.
package minisign

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

const (
	untrustedPrefix = "untrusted comment: "
	trustedPrefix   = "trusted comment: "
)

var (
	algPublicKey = [2]byte{'E', 'd'}
	algPrehashed = [2]byte{'E', 'D'}
)

// PublicKey is a minisign public key: an Ed25519 key plus minisign's 8-byte key id.
type PublicKey struct {
	KeyID [8]byte
	Key   ed25519.PublicKey
}

// Signature is a parsed .minisig file.
type Signature struct {
	Algorithm       [2]byte
	KeyID           [8]byte
	Signature       [64]byte
	TrustedComment  string
	GlobalSignature [64]byte
}

// ParsePublicKey parses a minisign public key file (two lines: an untrusted
// comment and the base64 key) or just the base64 key line.
func ParsePublicKey(data []byte) (PublicKey, error) {
	var pk PublicKey
	line, err := lastDataLine(data)
	if err != nil {
		return pk, fmt.Errorf("public key: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		return pk, fmt.Errorf("public key: invalid base64: %w", err)
	}
	if len(raw) != 2+8+ed25519.PublicKeySize {
		return pk, fmt.Errorf("public key: want %d bytes, got %d", 2+8+ed25519.PublicKeySize, len(raw))
	}
	if [2]byte(raw[:2]) != algPublicKey {
		return pk, errors.New("public key: unsupported algorithm")
	}
	copy(pk.KeyID[:], raw[2:10])
	pk.Key = ed25519.PublicKey(bytes.Clone(raw[10:]))
	return pk, nil
}

// ParseSignature parses a .minisig file.
func ParseSignature(data []byte) (Signature, error) {
	var sig Signature
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != 4 {
		return sig, fmt.Errorf("signature: want 4 lines, got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], untrustedPrefix) {
		return sig, errors.New("signature: missing untrusted comment")
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		return sig, fmt.Errorf("signature: invalid base64: %w", err)
	}
	if len(raw) != 2+8+64 {
		return sig, fmt.Errorf("signature: want %d bytes, got %d", 2+8+64, len(raw))
	}
	copy(sig.Algorithm[:], raw[:2])
	copy(sig.KeyID[:], raw[2:10])
	copy(sig.Signature[:], raw[10:])

	if !strings.HasPrefix(lines[2], trustedPrefix) {
		return sig, errors.New("signature: missing trusted comment")
	}
	sig.TrustedComment = strings.TrimPrefix(lines[2], trustedPrefix)

	global, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil {
		return sig, fmt.Errorf("signature: invalid global signature base64: %w", err)
	}
	if len(global) != 64 {
		return sig, fmt.Errorf("signature: global signature want 64 bytes, got %d", len(global))
	}
	copy(sig.GlobalSignature[:], global)
	return sig, nil
}

// Verify checks that sig is a valid signature of message by pk, including the
// trusted comment (which minisign signs separately).
func Verify(pk PublicKey, message []byte, sig Signature) error {
	if sig.Algorithm != algPrehashed {
		return errors.New("signature: only prehashed (ED) minisign signatures are accepted")
	}
	if sig.KeyID != pk.KeyID {
		return fmt.Errorf("signature: made by key %X, expected key %X", sig.KeyID, pk.KeyID)
	}
	digest := blake2b.Sum512(message)
	if !ed25519.Verify(pk.Key, digest[:], sig.Signature[:]) {
		return errors.New("signature: does not match the message")
	}
	global := append(bytes.Clone(sig.Signature[:]), sig.TrustedComment...)
	if !ed25519.Verify(pk.Key, global, sig.GlobalSignature[:]) {
		return errors.New("signature: trusted comment has been tampered with")
	}
	return nil
}

func lastDataLine(data []byte) (string, error) {
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, untrustedPrefix) {
			continue
		}
		return line, nil
	}
	return "", errors.New("no key data found")
}
