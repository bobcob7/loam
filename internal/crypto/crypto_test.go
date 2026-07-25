package crypto

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(seed byte) []byte {
	key := make([]byte, keySize)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

func TestNewEncryptor_KeyLength(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		keyLen  int
		wantErr bool
	}{
		{name: "zero bytes", keyLen: 0, wantErr: true},
		{name: "16 bytes (AES-128 size)", keyLen: 16, wantErr: true},
		{name: "31 bytes (one short)", keyLen: 31, wantErr: true},
		{name: "32 bytes (correct)", keyLen: 32, wantErr: false},
		{name: "33 bytes (one long)", keyLen: 33, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			enc, err := NewEncryptor(make([]byte, tt.keyLen))
			if tt.wantErr {
				require.ErrorIs(t, err, errInvalidKeySize)
				assert.Nil(t, enc)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, enc)
		})
	}
}

func TestEncryptor_RoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		plaintext []byte
	}{
		{name: "empty plaintext", plaintext: []byte{}},
		{name: "short ascii token", plaintext: []byte("ghp_abc123")},
		{name: "binary with null bytes", plaintext: []byte{0x00, 0x01, 0xff, 0x00, 0x7f}},
		{name: "long plaintext", plaintext: bytes.Repeat([]byte("token-material-"), 200)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			enc, err := NewEncryptor(testKey(1))
			require.NoError(t, err)
			ciphertext, err := enc.Encrypt(tt.plaintext)
			require.NoError(t, err)
			plaintext, err := enc.Decrypt(ciphertext)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(tt.plaintext, plaintext), "want %q, got %q", tt.plaintext, plaintext)
		})
	}
}

func TestEncryptor_Encrypt_NonceIsFreshEachCall(t *testing.T) {
	t.Parallel()
	enc, err := NewEncryptor(testKey(2))
	require.NoError(t, err)
	plaintext := []byte("the same secret token")
	first, err := enc.Encrypt(plaintext)
	require.NoError(t, err)
	second, err := enc.Encrypt(plaintext)
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "encrypting identical plaintext twice must yield different ciphertexts (nonce reuse)")
	firstPlain, err := enc.Decrypt(first)
	require.NoError(t, err)
	secondPlain, err := enc.Decrypt(second)
	require.NoError(t, err)
	assert.Equal(t, plaintext, firstPlain)
	assert.Equal(t, plaintext, secondPlain)
}

func TestEncryptor_Decrypt_TamperedCiphertextFails(t *testing.T) {
	t.Parallel()
	enc, err := NewEncryptor(testKey(3))
	require.NoError(t, err)
	original, err := enc.Encrypt([]byte("sensitive forge token"))
	require.NoError(t, err)
	for i := range original {
		region := "ciphertext-or-tag"
		if i < 12 {
			region = "nonce"
		}
		t.Run(fmt.Sprintf("%s byte %d of %d", region, i, len(original)), func(t *testing.T) {
			t.Parallel()
			tampered := make([]byte, len(original))
			copy(tampered, original)
			tampered[i] ^= 0xff
			_, err := enc.Decrypt(tampered)
			require.ErrorIs(t, err, errDecryptionFailed)
		})
	}
}

func TestEncryptor_Decrypt_WrongKeyFails(t *testing.T) {
	t.Parallel()
	encA, err := NewEncryptor(testKey(4))
	require.NoError(t, err)
	encB, err := NewEncryptor(testKey(5))
	require.NoError(t, err)
	ciphertext, err := encA.Encrypt([]byte("secret only encA should read"))
	require.NoError(t, err)
	_, err = encB.Decrypt(ciphertext)
	require.ErrorIs(t, err, errDecryptionFailed)
}

func TestEncryptor_Decrypt_TruncatedInputErrorsCleanly(t *testing.T) {
	t.Parallel()
	enc, err := NewEncryptor(testKey(6))
	require.NoError(t, err)
	tests := []struct {
		name string
		in   []byte
	}{
		{name: "empty input", in: []byte{}},
		{name: "one byte", in: []byte{0x01}},
		{name: "eleven bytes (one short of the 12-byte nonce)", in: make([]byte, 11)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := enc.Decrypt(tt.in)
			require.ErrorIs(t, err, errCiphertextTooShort)
		})
	}
}

func TestEncryptor_Encrypt_ProducesNonceLengthPrefix(t *testing.T) {
	t.Parallel()
	enc, err := NewEncryptor(testKey(7))
	require.NoError(t, err)
	plaintext := []byte("check the layout")
	ciphertext, err := enc.Encrypt(plaintext)
	require.NoError(t, err)
	const gcmNonceSize = 12
	const gcmTagSize = 16
	assert.Len(t, ciphertext, gcmNonceSize+len(plaintext)+gcmTagSize)
}

func TestEncryptor_Encrypt_NonceNotAllZero(t *testing.T) {
	t.Parallel()
	enc, err := NewEncryptor(testKey(8))
	require.NoError(t, err)
	zeroNonce := make([]byte, 12)
	ciphertext, err := enc.Encrypt(nil)
	require.NoError(t, err)
	assert.NotEqual(t, zeroNonce, ciphertext[:12], "crypto/rand should never hand back an all-zero nonce")
}
