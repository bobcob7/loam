// Package crypto encrypts secrets at rest with AES-256-GCM
// (docs/persistence-spec.md § Secrets). The only MVP secret is
// credentials.token_ciphertext (loam-54o.8); the key is a 32-byte value
// already base64-decoded and length-validated by internal/config from
// LOAM_ENCRYPTION_KEY — this package never reads the environment.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

const keySize = 32

var (
	// errInvalidKeySize means the key passed to NewEncryptor is not
	// exactly 32 bytes (AES-256). internal/config already enforces
	// this on LOAM_ENCRYPTION_KEY; NewEncryptor re-checks as a
	// constructor-level defence in depth.
	errInvalidKeySize = errors.New("crypto: key must be 32 bytes for AES-256")
	// errCiphertextTooShort means the input to Decrypt is shorter than
	// the prepended nonce, so it cannot be a value Encrypt produced.
	errCiphertextTooShort = errors.New("crypto: ciphertext shorter than nonce")
	// errDecryptionFailed means AES-GCM rejected the ciphertext: wrong
	// key, wrong nonce, or the ciphertext (including the prepended
	// nonce) was tampered with after encryption.
	errDecryptionFailed = errors.New("crypto: decryption failed")
)

// Encryptor encrypts and decrypts secrets at rest with AES-256-GCM. Each
// Encrypt call draws a fresh random nonce from crypto/rand and prepends
// it to the returned ciphertext; Decrypt splits that same prefix back
// off before authenticating and opening the remainder.
type Encryptor struct {
	aead cipher.AEAD
}

// NewEncryptor builds an Encryptor from an already-decoded AES-256 key.
// Callers (the credentials store, loam-54o.8) get key from
// internal/config's Config.EncryptionKey, which decodes
// LOAM_ENCRYPTION_KEY from base64 and validates its length; NewEncryptor
// does not read the environment or perform any decoding itself.
func NewEncryptor(key []byte) (*Encryptor, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("crypto: key is %d bytes: %w", len(key), errInvalidKeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: building AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: building GCM mode: %w", err)
	}
	return &Encryptor{aead: aead}, nil
}

// Encrypt seals plaintext and returns nonce||ciphertext||tag: a fresh
// random nonce (aead.NonceSize() bytes, currently 12) prepended to the
// AES-GCM sealed output, which itself ends with a 16-byte authentication
// tag. The nonce is not secret and is safe to store alongside the
// ciphertext; Decrypt expects exactly this layout.
func (e *Encryptor) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: generating nonce: %w", err)
	}
	return e.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt: it splits the leading nonce off ciphertext,
// then authenticates and opens the remainder. It returns an error
// wrapping errCiphertextTooShort if ciphertext is shorter than the
// nonce, or one wrapping errDecryptionFailed if authentication fails —
// a tampered ciphertext (including a modified nonce), the wrong key, or
// otherwise-corrupt input.
func (e *Encryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	nonceSize := e.aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("crypto: ciphertext is %d bytes, need at least %d: %w", len(ciphertext), nonceSize, errCiphertextTooShort)
	}
	nonce, sealed := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := e.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: opening ciphertext: %w: %w", errDecryptionFailed, err)
	}
	return plaintext, nil
}
