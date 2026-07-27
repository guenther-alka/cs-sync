// Encryption layer for the cs-sync wire protocol. cs-sync 2.0 relied on an
// external cs-stream tunnel for encryption; this makes cs-sync self-
// sufficient (Gea request 2026.07.25: "cs-sync soll autark ohne cs-stream
// verschluesseln"). Every frame's payload is sealed with ChaCha20-Poly1305
// AEAD, keyed from the existing 20-char transfer key (same value already
// used for the pre-shared-key check, hashed to a 32-byte key via SHA-256).
// Frame type byte is passed as AEAD associated data so a frame can't be
// silently reinterpreted as a different type by an on-path attacker.
package wire

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// DeriveKey turns the 20-char transfer key into a 32-byte ChaCha20-Poly1305
// key. SHA-256 is used purely as a KDF-ish stretch, not for any security
// property beyond fixed-length uniform key material -- the real secret
// strength comes from the shared server.auth value already trusted
// cluster-wide (same value used for the group/*.txt SSH key distribution).
func DeriveKey(transferKey string) [32]byte {
	return sha256.Sum256([]byte(transferKey))
}

// newAEAD builds the ChaCha20-Poly1305 cipher for a session key.
func newAEAD(key [32]byte) (cipherAEAD, error) {
	return chacha20poly1305.New(key[:])
}

// cipherAEAD is the minimal interface implemented by chacha20poly1305.
type cipherAEAD interface {
	NonceSize() int
	Overhead() int
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}

func seal(a cipherAEAD, frameType byte, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("wire: nonce: %w", err)
	}
	out := a.Seal(nonce, nonce, plaintext, []byte{frameType})
	return out, nil
}

func open(a cipherAEAD, frameType byte, sealed []byte) ([]byte, error) {
	ns := a.NonceSize()
	if len(sealed) < ns {
		return nil, fmt.Errorf("wire: sealed frame too short")
	}
	nonce, ct := sealed[:ns], sealed[ns:]
	return a.Open(nil, nonce, ct, []byte{frameType})
}
