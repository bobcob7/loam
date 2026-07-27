// Package credentialstore implements the credentials aggregate: token-only
// per docs/persistence-spec.md "credentials" -- id, host (unique),
// token_ciphertext (bytea, null), validated (bool), timestamps. There is
// no ssh_private_key_ciphertext and no ssh_public_key column anywhere in
// this package or the schema it reads; this bead's own DESCRIPTION and
// ACCEPTANCE CRITERIA still name SSH columns, but that is leftover
// pre-correction text superseded by the DESIGN's spec correction -- see
// loam-54o.8's notes.
//
// UpsertToken and GetByHost are the only methods that ever see plaintext:
// UpsertToken encrypts the caller's plaintext token through the injected
// AES-GCM encryptor (loam-54o.6, internal/crypto) before it reaches any
// query, and GetByHost decrypts token_ciphertext back to plaintext for
// callers that need it (git credential injection, docs/sync-spec.md
// "Upstream Transport"; forge REST calls). GetStatus and ListStatuses back
// CredentialService.GetCredentialStatus/ListCredentials
// (docs/web-spec.md "CredentialService") and never decrypt: has_token is
// computed as token_ciphertext IS NOT NULL at query time, not read from a
// separate stored flag. SetValidated updates the validated flag set by the
// provider's ValidateToken result and never touches token_ciphertext
// either.
package credentialstore

import (
	"context"

	"github.com/bobcob7/loam/internal/db/gen"
)

//go:generate go tool moq -out moq_test.go . querier encryptor

// querier is the sqlc-generated surface Store calls, defined here at the
// consumer per repo convention so Store is unit-testable against a moq
// mock instead of a live database; *gen.Queries satisfies it unmodified,
// whether bound to a *pgxpool.Pool (New) or a pgx.Tx (NewInTx).
type querier interface {
	UpsertCredentialToken(ctx context.Context, arg gen.UpsertCredentialTokenParams) (gen.Credential, error)
	GetCredentialByHost(ctx context.Context, host string) (gen.Credential, error)
	GetCredentialStatus(ctx context.Context, host string) (gen.GetCredentialStatusRow, error)
	ListCredentialStatuses(ctx context.Context) ([]gen.ListCredentialStatusesRow, error)
	SetCredentialValidated(ctx context.Context, arg gen.SetCredentialValidatedParams) (gen.SetCredentialValidatedRow, error)
}

// encryptor is the AES-GCM encrypt/decrypt surface Store needs from
// internal/crypto (loam-54o.6), defined here at the consumer so Store is
// unit-testable against a moq mock instead of a real cipher --
// *crypto.Encryptor satisfies it unmodified. Every plaintext token that
// reaches this package leaves it only through Encrypt's return value; every
// plaintext token Store ever hands back to a caller came only from
// Decrypt's return value.
type encryptor interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}
