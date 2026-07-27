package credentialstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/db/gen"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// xorEncryptor is a fake encryptor standing in for internal/crypto's real
// AES-GCM Encryptor: Encrypt/Decrypt both XOR every byte against key,
// which is its own inverse, so round-tripping through both proves the
// store actually calls Decrypt (not just Encrypt) without needing a real
// cipher in a unit test. It is deliberately NOT the identity function: a
// mutation that returns the ciphertext bytes unmodified from GetByHost
// (skipping Decrypt) is caught because xorEncryptor's "ciphertext" is
// never equal to the plaintext it was built from (key is non-zero).
type xorEncryptor struct {
	key             byte
	encryptCalls    int
	decryptCalls    int
	forceDecryptErr error
}

func (e *xorEncryptor) Encrypt(plaintext []byte) ([]byte, error) {
	e.encryptCalls++
	out := make([]byte, len(plaintext))
	for i, b := range plaintext {
		out[i] = b ^ e.key
	}
	return out, nil
}

func (e *xorEncryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	e.decryptCalls++
	if e.forceDecryptErr != nil {
		return nil, e.forceDecryptErr
	}
	out := make([]byte, len(ciphertext))
	for i, b := range ciphertext {
		out[i] = b ^ e.key
	}
	return out, nil
}

func TestUpsertToken_EncryptsBeforeCallingQuerier(t *testing.T) {
	t.Parallel()
	enc := &xorEncryptor{key: 0x5a}
	var capturedCiphertext []byte
	q := &querierMock{
		UpsertCredentialTokenFunc: func(_ context.Context, arg gen.UpsertCredentialTokenParams) (gen.Credential, error) {
			capturedCiphertext = arg.TokenCiphertext
			assert.Equal(t, "github.com", arg.Host)
			return gen.Credential{
				ID:              arg.ID,
				Host:            arg.Host,
				TokenCiphertext: arg.TokenCiphertext,
				Validated:       false,
				CreatedAt:       pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
				UpdatedAt:       pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
			}, nil
		},
	}
	s := newStore(q, enc, testLogger())
	status, err := s.UpsertToken(t.Context(), "github.com", "ghp_supersecret")
	require.NoError(t, err)
	assert.Equal(t, 1, enc.encryptCalls, "UpsertToken must encrypt exactly once")
	assert.NotEqual(t, []byte("ghp_supersecret"), capturedCiphertext,
		"the querier must never see the plaintext token -- only Encrypt's output")
	assert.Equal(t, "github.com", status.Host)
	assert.True(t, status.HasToken)
	assert.False(t, status.Validated)
}

func TestUpsertToken_EncryptError_PropagatesAndNeverCallsQuerier(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom")
	enc := &encryptorMock{
		EncryptFunc: func(_ []byte) ([]byte, error) { return nil, wantErr },
	}
	q := &querierMock{
		// Deliberately configured (not left nil) so an implementation
		// that skips the Encrypt-error short-circuit and reaches the
		// querier anyway fails this test via a normal assertion --
		// never via moq's panic-on-unset-func fallback, which would
		// abort the whole test binary instead of just this test.
		UpsertCredentialTokenFunc: func(_ context.Context, _ gen.UpsertCredentialTokenParams) (gen.Credential, error) {
			t.Error("UpsertToken must not call the querier when Encrypt fails")
			return gen.Credential{}, nil
		},
	}
	s := newStore(q, enc, testLogger())
	_, err := s.UpsertToken(t.Context(), "github.com", "ghp_supersecret")
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

func TestGetByHost_DecryptsCiphertext_PlaintextMatchesOriginal(t *testing.T) {
	t.Parallel()
	enc := &xorEncryptor{key: 0x42}
	plaintext := "ghp_realtokenvalue"
	ciphertext, err := enc.Encrypt([]byte(plaintext))
	require.NoError(t, err)
	require.NotEqual(t, []byte(plaintext), ciphertext, "fixture sanity: the ciphertext must not equal the plaintext")
	id := uuid.New()
	q := &querierMock{
		GetCredentialByHostFunc: func(_ context.Context, host string) (gen.Credential, error) {
			assert.Equal(t, "github.com", host)
			return gen.Credential{
				ID:              pgUUID(id),
				Host:            host,
				TokenCiphertext: ciphertext,
				Validated:       true,
				CreatedAt:       pgtype.Timestamptz{Time: time.Unix(100, 0), Valid: true},
				UpdatedAt:       pgtype.Timestamptz{Time: time.Unix(200, 0), Valid: true},
			}, nil
		},
	}
	s := newStore(q, enc, testLogger())
	cred, err := s.GetByHost(t.Context(), "github.com")
	require.NoError(t, err)
	assert.Equal(t, 1, enc.decryptCalls, "GetByHost must decrypt exactly once")
	assert.Equal(t, plaintext, cred.Token, "the decrypted plaintext must match what was encrypted")
	assert.NotEqual(t, string(ciphertext), cred.Token, "the returned token must never be the raw ciphertext")
	assert.Equal(t, id, cred.ID)
	assert.True(t, cred.Validated)
}

func TestGetByHost_DecryptFails_ReturnsErrorNotGarbage(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("crypto: decryption failed")
	enc := &xorEncryptor{key: 0x11, forceDecryptErr: wantErr}
	q := &querierMock{
		GetCredentialByHostFunc: func(_ context.Context, host string) (gen.Credential, error) {
			return gen.Credential{
				ID:              pgUUID(uuid.New()),
				Host:            host,
				TokenCiphertext: []byte{1, 2, 3, 4},
				CreatedAt:       pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
				UpdatedAt:       pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
			}, nil
		},
	}
	s := newStore(q, enc, testLogger())
	cred, err := s.GetByHost(t.Context(), "github.com")
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Empty(t, cred.Token, "a failed decryption must never leak a non-empty Token")
}

func TestGetByHost_NoRows_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	enc := &xorEncryptor{key: 0x01}
	q := &querierMock{
		GetCredentialByHostFunc: func(_ context.Context, _ string) (gen.Credential, error) {
			return gen.Credential{}, pgx.ErrNoRows
		},
	}
	s := newStore(q, enc, testLogger())
	_, err := s.GetByHost(t.Context(), "unknown.example.com")
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

func TestGetByHost_NullTokenCiphertext_ReturnsErrNoTokenWithoutCallingDecrypt(t *testing.T) {
	t.Parallel()
	enc := &encryptorMock{
		// Deliberately configured so a mistaken Decrypt call fails via a
		// normal assertion rather than moq's unset-func panic.
		DecryptFunc: func(_ []byte) ([]byte, error) {
			t.Error("GetByHost must not call Decrypt when token_ciphertext is nil")
			return nil, nil
		},
	}
	q := &querierMock{
		GetCredentialByHostFunc: func(_ context.Context, host string) (gen.Credential, error) {
			return gen.Credential{
				ID:              pgUUID(uuid.New()),
				Host:            host,
				TokenCiphertext: nil,
				CreatedAt:       pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
				UpdatedAt:       pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
			}, nil
		},
	}
	s := newStore(q, enc, testLogger())
	_, err := s.GetByHost(t.Context(), "host-with-no-token.example.com")
	require.Error(t, err)
	assert.ErrorIs(t, err, errNoToken)
}

func TestGetStatus_NeverCallsEncryptor(t *testing.T) {
	t.Parallel()
	enc := &encryptorMock{
		EncryptFunc: func(_ []byte) ([]byte, error) { t.Error("GetStatus must not call Encrypt"); return nil, nil },
		DecryptFunc: func(_ []byte) ([]byte, error) { t.Error("GetStatus must not call Decrypt"); return nil, nil },
	}
	q := &querierMock{
		GetCredentialStatusFunc: func(_ context.Context, host string) (gen.GetCredentialStatusRow, error) {
			return gen.GetCredentialStatusRow{Host: host, HasToken: true, Validated: false}, nil
		},
	}
	s := newStore(q, enc, testLogger())
	status, err := s.GetStatus(t.Context(), "github.com")
	require.NoError(t, err)
	assert.Equal(t, CredentialStatus{Host: "github.com", HasToken: true, Validated: false}, status)
}

func TestGetStatus_NoRows_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	enc := &encryptorMock{}
	q := &querierMock{
		GetCredentialStatusFunc: func(_ context.Context, _ string) (gen.GetCredentialStatusRow, error) {
			return gen.GetCredentialStatusRow{}, pgx.ErrNoRows
		},
	}
	s := newStore(q, enc, testLogger())
	_, err := s.GetStatus(t.Context(), "unknown.example.com")
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

func TestListStatuses_NeverCallsEncryptor_ReturnsEveryHost(t *testing.T) {
	t.Parallel()
	enc := &encryptorMock{
		EncryptFunc: func(_ []byte) ([]byte, error) { t.Error("ListStatuses must not call Encrypt"); return nil, nil },
		DecryptFunc: func(_ []byte) ([]byte, error) { t.Error("ListStatuses must not call Decrypt"); return nil, nil },
	}
	q := &querierMock{
		ListCredentialStatusesFunc: func(_ context.Context) ([]gen.ListCredentialStatusesRow, error) {
			return []gen.ListCredentialStatusesRow{
				{Host: "github.com", HasToken: true, Validated: true},
				{Host: "forgejo.example.com", HasToken: false, Validated: false},
			}, nil
		},
	}
	s := newStore(q, enc, testLogger())
	statuses, err := s.ListStatuses(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []CredentialStatus{
		{Host: "github.com", HasToken: true, Validated: true},
		{Host: "forgejo.example.com", HasToken: false, Validated: false},
	}, statuses)
}

func TestListStatuses_Empty_ReturnsEmptyNotNilError(t *testing.T) {
	t.Parallel()
	enc := &encryptorMock{}
	q := &querierMock{
		ListCredentialStatusesFunc: func(_ context.Context) ([]gen.ListCredentialStatusesRow, error) {
			return nil, nil
		},
	}
	s := newStore(q, enc, testLogger())
	statuses, err := s.ListStatuses(t.Context())
	require.NoError(t, err)
	assert.Empty(t, statuses)
}

func TestSetValidated_NeverCallsEncryptor_UpdatesFlag(t *testing.T) {
	t.Parallel()
	enc := &encryptorMock{
		EncryptFunc: func(_ []byte) ([]byte, error) { t.Error("SetValidated must not call Encrypt"); return nil, nil },
		DecryptFunc: func(_ []byte) ([]byte, error) { t.Error("SetValidated must not call Decrypt"); return nil, nil },
	}
	q := &querierMock{
		SetCredentialValidatedFunc: func(_ context.Context, arg gen.SetCredentialValidatedParams) (gen.SetCredentialValidatedRow, error) {
			assert.Equal(t, "github.com", arg.Host)
			assert.True(t, arg.Validated)
			return gen.SetCredentialValidatedRow{Host: arg.Host, HasToken: true, Validated: arg.Validated}, nil
		},
	}
	s := newStore(q, enc, testLogger())
	status, err := s.SetValidated(t.Context(), "github.com", true)
	require.NoError(t, err)
	assert.True(t, status.Validated)
}

func TestSetValidated_NoRows_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	enc := &encryptorMock{}
	q := &querierMock{
		SetCredentialValidatedFunc: func(_ context.Context, _ gen.SetCredentialValidatedParams) (gen.SetCredentialValidatedRow, error) {
			return gen.SetCredentialValidatedRow{}, pgx.ErrNoRows
		},
	}
	s := newStore(q, enc, testLogger())
	_, err := s.SetValidated(t.Context(), "unknown.example.com", true)
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}
