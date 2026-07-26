// Package crypto encrypts secrets at rest with AES-256-GCM
// (docs/persistence-spec.md § Secrets). The only MVP secret is
// credentials.token_ciphertext (loam-54o.8); the key is a 32-byte value
// already base64-decoded and length-validated by internal/config from
// LOAM_ENCRYPTION_KEY — this package never reads the environment.
//
// Stored blobs are laid out as version(1) || nonce(12) || sealed(plaintext+tag),
// where sealed's trailing 16 bytes are the AES-GCM authentication tag; see
// the Encryptor doc comment for why the leading version byte is there.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

const (
	keySize = 32
	// versionSize is the width of the format-version prefix Encrypt
	// writes and Decrypt reads. See "Format versioning" below.
	versionSize = 1
	// formatVersion1 is the only format Encrypt currently produces:
	// version(1) || nonce(aead.NonceSize()) || sealed(plaintext+tag).
	formatVersion1 byte = 1
)

var (
	// errInvalidKeySize means the key passed to NewEncryptor is not
	// exactly 32 bytes (AES-256). internal/config already enforces
	// this on LOAM_ENCRYPTION_KEY; NewEncryptor re-checks as a
	// constructor-level defence in depth.
	errInvalidKeySize = errors.New("crypto: key must be 32 bytes for AES-256")
	// errCiphertextTooShort means the input to Decrypt is shorter than
	// the version byte plus nonce, so it cannot be a value Encrypt
	// produced.
	errCiphertextTooShort = errors.New("crypto: ciphertext shorter than version prefix and nonce")
	// errUnsupportedVersion means the leading version byte is not one
	// Decrypt knows how to parse. This is deliberately distinct from
	// errDecryptionFailed: the version byte is public framing, not
	// secret material, so rejecting it early leaks nothing about the
	// key and is not a decryption oracle. It signals "this blob is a
	// different format" (e.g. written by a newer binary, or under a
	// KMS-backed envelope scheme) rather than "this key/ciphertext
	// pairing is wrong."
	errUnsupportedVersion = errors.New("crypto: unsupported ciphertext format version")
	// errDecryptionFailed means AES-GCM rejected the ciphertext: wrong
	// key, wrong nonce, or the ciphertext (including the prepended
	// nonce) was tampered with after encryption. Deliberately a single
	// sentinel for all three causes — see "Format versioning" below.
	errDecryptionFailed = errors.New("crypto: decryption failed")
)

// Encryptor encrypts and decrypts secrets at rest with AES-256-GCM. Each
// Encrypt call draws a fresh random nonce from crypto/rand and prepends
// it, behind a leading format-version byte, to the returned ciphertext;
// Decrypt splits that same prefix back off before authenticating and
// opening the remainder.
//
// # Format versioning
//
// This was decided deliberately (loam-9r4), not left as a gap, before
// loam-54o.8 writes the first credentials row. Three options were on the
// table:
//
//  1. A leading version byte in the blob itself (chosen — see below).
//  2. A `key_version` column alongside `token_ciphertext`, queryable
//     without opening any blob ("which rows are still on v1").
//  3. No format marker at all: on Decrypt failure, retry with each
//     known key/algorithm in turn ("trial decryption").
//
// (2) and (3) are both about identifying *which key* produced a blob,
// and both were rejected for that job for the same reason: the
// `credentials` table is keyed by unique host (docs/persistence-spec.md
// § credentials) and will hold single-digit rows in any real deployment,
// so if a second key source (LOAM_ENCRYPTION_KEY today, a KMS later,
// per docs/persistence-spec.md § Secrets) or a rotated key is ever in
// play, trying each candidate key against each of a handful of rows is
// cheap — genuinely defensible, not a shortcut. Nothing here forecloses
// that: NewEncryptor takes one key, so key rotation is a caller-side
// concern (loam-54o.8 constructing an Encryptor per candidate key and
// trying each on decryption failure), and this package intentionally
// does not grow multi-key machinery to support it.
//
// The one-byte version prefix solves a different, narrower problem:
// telling *which format/algorithm* wrote a blob, cheaply, before any
// key is chosen. That distinction matters because a KMS-backed envelope
// (persistence-spec's other named key source) is very unlikely to be
// "AES-256-GCM with a bare 32-byte key" — it may carry its own wrapped
// key or IV framing. Without a version byte, distinguishing an
// AES-GCM-direct blob from a future KMS-envelope blob requires either
// trying both parsers on every read or an out-of-band column anyway.
// One byte of insurance, written once, is orders of magnitude cheaper
// than that backfill+dual-read-path later, which is why it is worth
// doing now even though the reviewer who raised this (2026-07-25) noted
// the cost of being wrong is low either way.
//
// If loam-54o.8 later needs `key_version` for the key-rotation case
// above, it is additive to this: a DB column recording which key
// candidate decrypted a row, orthogonal to the format byte this package
// owns.
//
// One gap this leaves, tracked as loam-nqy rather than fixed here: a
// caller running the trial-decryption loop above cannot yet distinguish
// "wrong key, try the next one" from "wrong parser, no key will help"
// (errUnsupportedVersion) without an exported sentinel, so it would
// grind through every candidate key on a blob it structurally cannot
// read. Both sentinels stay unexported until loam-54o.8 has a caller
// that actually needs to branch on them — exporting speculatively, with
// no consumer, would violate unexported-by-default.
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

// Encrypt seals plaintext and returns version||nonce||ciphertext||tag: a
// single format-version byte (currently always formatVersion1), followed
// by a fresh random nonce (aead.NonceSize() bytes, currently 12), followed
// by the AES-GCM sealed output, which itself ends with a 16-byte
// authentication tag. Neither the version byte nor the nonce is secret;
// both are safe to store alongside the ciphertext. Decrypt expects
// exactly this layout.
func (e *Encryptor) Encrypt(plaintext []byte) ([]byte, error) {
	nonceSize := e.aead.NonceSize()
	buf := make([]byte, versionSize+nonceSize, versionSize+nonceSize+len(plaintext)+e.aead.Overhead())
	buf[0] = formatVersion1
	nonce := buf[versionSize:]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: generating nonce: %w", err)
	}
	return e.aead.Seal(buf, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt: it reads the leading version byte, splits
// the nonce that follows it off the remaining ciphertext, then
// authenticates and opens what is left. It returns an error wrapping:
//
//   - errCiphertextTooShort if ciphertext is shorter than the version
//     byte plus nonce combined — checked first, before the version byte
//     is even read, so truncated input never panics;
//   - errUnsupportedVersion if the version byte is not one Decrypt
//     recognizes (a distinct condition from a decryption failure — see
//     the Encryptor doc comment's "Format versioning" section);
//   - errDecryptionFailed if AES-GCM authentication fails — a tampered
//     ciphertext (including a modified nonce), the wrong key, or
//     otherwise-corrupt input. These causes are deliberately
//     indistinguishable in the returned error so Decrypt is not a
//     decryption oracle.
func (e *Encryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	nonceSize := e.aead.NonceSize()
	prefixSize := versionSize + nonceSize
	if len(ciphertext) < prefixSize {
		return nil, fmt.Errorf("crypto: ciphertext is %d bytes, need at least %d: %w", len(ciphertext), prefixSize, errCiphertextTooShort)
	}
	if version := ciphertext[0]; version != formatVersion1 {
		return nil, fmt.Errorf("crypto: ciphertext has version %d: %w", version, errUnsupportedVersion)
	}
	nonce, sealed := ciphertext[versionSize:prefixSize], ciphertext[prefixSize:]
	plaintext, err := e.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: opening ciphertext: %w: %w", errDecryptionFailed, err)
	}
	return plaintext, nil
}
