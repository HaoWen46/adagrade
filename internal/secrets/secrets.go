// Package secrets encrypts at-rest credentials (LLM API keys entered in the app)
// with AES-256-GCM under a machine-local master key.
//
// The master key is a 32-byte file the app creates on first boot (0600, default
// ./data/secret.key — docs/DECISIONS.md D16). This keeps day-to-day key management
// inside the app UI with no env editing: the only secret on disk is one generated
// file. Losing it is recoverable (re-enter API keys in the UI); leaking it together
// with a DB dump exposes the stored keys, so back it up like the database.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/hkdf"
)

// LoadOrCreateKey returns the master key at path, generating it (and parent dirs)
// on first use. A present-but-malformed file is an error — never silently replaced,
// since replacing it would orphan every stored credential.
func LoadOrCreateKey(path string) ([32]byte, error) {
	var key [32]byte
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(raw) != 32 {
			return key, fmt.Errorf("secrets: key file %s is %d bytes, want 32 — refusing to overwrite; delete it manually to reset (stored API keys become unreadable)", path, len(raw))
		}
		copy(key[:], raw)
		return key, nil
	case errors.Is(err, os.ErrNotExist):
		if _, err := rand.Read(key[:]); err != nil {
			return key, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return key, err
		}
		if err := os.WriteFile(path, key[:], 0o600); err != nil {
			return key, err
		}
		return key, nil
	default:
		return key, fmt.Errorf("secrets: read key file: %w", err)
	}
}

// Seal encrypts plaintext; output is nonce||ciphertext.
func Seal(key [32]byte, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts a Seal output.
func Open(key [32]byte, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("secrets: ciphertext too short")
	}
	pt, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return nil, errors.New("secrets: decryption failed (tampered data or wrong master key)")
	}
	return pt, nil
}

// Derive returns a 32-byte subkey from the master key via HKDF-SHA256, scoped by
// info so distinct purposes (e.g. "regrade-token-v1") never share key material —
// a leaked subkey for one purpose does not compromise another, and the master key
// itself is never used directly outside Seal/Open. Deterministic: the same
// (key, info) pair always yields the same subkey, which the regrade-token verifier
// relies on (recomputation, not lookup).
func Derive(key [32]byte, info string) []byte {
	r := hkdf.New(sha256.New, key[:], nil, []byte(info))
	sub := make([]byte, 32)
	// hkdf.New's Reader never errors for a request within its expand-length limit
	// (255*hash size); 32 bytes is far under that, so this can't fail in practice.
	if _, err := io.ReadFull(r, sub); err != nil {
		panic("secrets: hkdf expand failed: " + err.Error())
	}
	return sub
}

func newGCM(key [32]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
