package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/credentialstore"
)

// TestVerifyStoredCredentialsDecrypt_NoCredentials_SucceedsWithoutLookup
// covers a fresh install: zero credential rows means nothing to verify, and
// this must never call GetByHost at all -- a stub GetByHostFunc panics if
// invoked (see forgeCredentialLookupMock's own doc comment), so an
// unexpected call fails the test immediately rather than silently.
func TestVerifyStoredCredentialsDecrypt_NoCredentials_SucceedsWithoutLookup(t *testing.T) {
	t.Parallel()
	lister := &credentialListerMock{
		ListStatusesFunc: func(ctx context.Context) ([]credentialstore.CredentialStatus, error) {
			return nil, nil
		},
	}
	lookup := &forgeCredentialLookupMock{}
	err := verifyStoredCredentialsDecrypt(t.Context(), lister, lookup, testLogger())
	require.NoError(t, err)
	assert.Empty(t, lookup.GetByHostCalls())
}

// TestVerifyStoredCredentialsDecrypt_ListingFails_ReturnsWrappedError proves
// a failure enumerating hosts (a genuine database problem, distinct from a
// decryption failure) is reported rather than swallowed.
func TestVerifyStoredCredentialsDecrypt_ListingFails_ReturnsWrappedError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("connection reset")
	lister := &credentialListerMock{
		ListStatusesFunc: func(ctx context.Context) ([]credentialstore.CredentialStatus, error) {
			return nil, wantErr
		},
	}
	err := verifyStoredCredentialsDecrypt(t.Context(), lister, &forgeCredentialLookupMock{}, testLogger())
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

// TestVerifyStoredCredentialsDecrypt_HostWithoutToken_SkipsLookup proves a
// host with no stored token never reaches GetByHost -- there is nothing to
// decrypt, and credentialstore.Store.GetByHost would answer ErrNoToken for
// exactly this case, so calling it at all would be pure waste.
func TestVerifyStoredCredentialsDecrypt_HostWithoutToken_SkipsLookup(t *testing.T) {
	t.Parallel()
	lister := &credentialListerMock{
		ListStatusesFunc: func(ctx context.Context) ([]credentialstore.CredentialStatus, error) {
			return []credentialstore.CredentialStatus{{Host: "forgejo.example.com", HasToken: false}}, nil
		},
	}
	lookup := &forgeCredentialLookupMock{}
	err := verifyStoredCredentialsDecrypt(t.Context(), lister, lookup, testLogger())
	require.NoError(t, err)
	assert.Empty(t, lookup.GetByHostCalls())
}

// TestVerifyStoredCredentialsDecrypt_AllHostsDecrypt_Succeeds is the happy
// path: every host with a token decrypts cleanly under the booting key, so
// startup must proceed.
func TestVerifyStoredCredentialsDecrypt_AllHostsDecrypt_Succeeds(t *testing.T) {
	t.Parallel()
	hosts := []string{"forgejo.a.example.com", "forgejo.b.example.com"}
	lister := &credentialListerMock{
		ListStatusesFunc: func(ctx context.Context) ([]credentialstore.CredentialStatus, error) {
			return []credentialstore.CredentialStatus{
				{Host: hosts[0], HasToken: true},
				{Host: hosts[1], HasToken: true},
			}, nil
		},
	}
	lookup := &forgeCredentialLookupMock{
		GetByHostFunc: func(ctx context.Context, host string) (credentialstore.Credential, error) {
			return credentialstore.Credential{Host: host, Token: "irrelevant-decrypted-plaintext"}, nil
		},
	}
	err := verifyStoredCredentialsDecrypt(t.Context(), lister, lookup, testLogger())
	require.NoError(t, err)
	require.Len(t, lookup.GetByHostCalls(), 2)
	assert.ElementsMatch(t, hosts, []string{lookup.GetByHostCalls()[0].Host, lookup.GetByHostCalls()[1].Host})
}

// TestVerifyStoredCredentialsDecrypt_DecryptionFails_ReturnsFatalErrorNamingHost
// is this bead's central proof at the orchestration level: a host whose
// stored ciphertext GetByHost cannot open -- exactly what
// internal/credentialstore.Store.GetByHost returns when internal/crypto's
// Decrypt fails under a mismatched LOAM_ENCRYPTION_KEY -- must fail this
// check, naming the host, rather than being reported as healthy the way
// CredentialService.GetCredentialStatus/ListCredentials do (loam-0ab's
// actual bug). The underlying error is a crypto-shaped message, standing in
// for the real internal/crypto.Decrypt failure without needing a real
// cipher in this fast, DB-free unit test -- internal/crypto's own tests
// and internal/credentialstore's own xorEncryptor-based tests already cover
// that the failure genuinely propagates this far.
func TestVerifyStoredCredentialsDecrypt_DecryptionFails_ReturnsFatalErrorNamingHost(t *testing.T) {
	t.Parallel()
	const badHost = "forgejo.wrongkey.example.com"
	decryptErr := errors.New("crypto: opening ciphertext: crypto: decryption failed: cipher: message authentication failed")
	lister := &credentialListerMock{
		ListStatusesFunc: func(ctx context.Context) ([]credentialstore.CredentialStatus, error) {
			return []credentialstore.CredentialStatus{{Host: badHost, HasToken: true}}, nil
		},
	}
	lookup := &forgeCredentialLookupMock{
		GetByHostFunc: func(ctx context.Context, host string) (credentialstore.Credential, error) {
			return credentialstore.Credential{}, decryptErr
		},
	}
	err := verifyStoredCredentialsDecrypt(t.Context(), lister, lookup, testLogger())
	require.Error(t, err)
	assert.ErrorIs(t, err, decryptErr)
	assert.Contains(t, err.Error(), badHost, "the fatal error must name which host's credential could not be decrypted, or an operator has nothing to act on")
}

// TestVerifyStoredCredentialsDecrypt_FirstFailureStopsShortOfLaterHosts
// proves this fails fast on the FIRST bad host rather than exhausting the
// whole list -- one wrong key affects every row uniformly (there is only
// ever one LOAM_ENCRYPTION_KEY per process), so there is nothing gained by
// continuing, and continuing would mean an unnecessary Decrypt attempt per
// remaining host.
func TestVerifyStoredCredentialsDecrypt_FirstFailureStopsShortOfLaterHosts(t *testing.T) {
	t.Parallel()
	lister := &credentialListerMock{
		ListStatusesFunc: func(ctx context.Context) ([]credentialstore.CredentialStatus, error) {
			return []credentialstore.CredentialStatus{
				{Host: "forgejo.first.example.com", HasToken: true},
				{Host: "forgejo.second.example.com", HasToken: true},
			}, nil
		},
	}
	lookup := &forgeCredentialLookupMock{
		GetByHostFunc: func(ctx context.Context, host string) (credentialstore.Credential, error) {
			return credentialstore.Credential{}, errors.New("decryption failed")
		},
	}
	err := verifyStoredCredentialsDecrypt(t.Context(), lister, lookup, testLogger())
	require.Error(t, err)
	assert.Len(t, lookup.GetByHostCalls(), 1, "the first failing host is enough to abort startup; there is no reason to try the rest")
}

// TestVerifyStoredCredentialsDecrypt_NotFoundRace_IsSkippedNotFatal proves
// the one exception this check makes: ErrNotFound (the row this host's
// status was read from has already disappeared by the time GetByHost
// re-reads it) is a lost race with nothing left to verify, not a key
// problem, and must not abort startup.
func TestVerifyStoredCredentialsDecrypt_NotFoundRace_IsSkippedNotFatal(t *testing.T) {
	t.Parallel()
	lister := &credentialListerMock{
		ListStatusesFunc: func(ctx context.Context) ([]credentialstore.CredentialStatus, error) {
			return []credentialstore.CredentialStatus{{Host: "forgejo.example.com", HasToken: true}}, nil
		},
	}
	lookup := &forgeCredentialLookupMock{
		GetByHostFunc: func(ctx context.Context, host string) (credentialstore.Credential, error) {
			return credentialstore.Credential{}, credentialstore.ErrNotFound
		},
	}
	err := verifyStoredCredentialsDecrypt(t.Context(), lister, lookup, testLogger())
	assert.NoError(t, err)
}
