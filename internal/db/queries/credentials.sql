-- name: UpsertCredentialToken :one
-- Sets or replaces host's token: an insert on the first call, an update in
-- place (never a second row) on every later call for the same host --
-- credentials_host_key (UNIQUE(host), docs/persistence-spec.md
-- "credentials") is the constraint this upsert relies on. $3 is the
-- already-encrypted token_ciphertext: the plaintext token never reaches
-- this query (internal/credentialstore's Store encrypts it via the
-- injected AES-GCM encryptor before calling this).
INSERT INTO credentials (id, host, token_ciphertext)
VALUES ($1, $2, $3)
ON CONFLICT (host) DO UPDATE SET token_ciphertext = EXCLUDED.token_ciphertext, updated_at = now()
RETURNING *;

-- name: GetCredentialByHost :one
-- The full row, ciphertext included, for callers that need the decrypted
-- plaintext token (git credential injection, forge REST calls --
-- docs/sync-spec.md "Upstream Transport"). Decryption happens in
-- internal/credentialstore, never here.
SELECT * FROM credentials WHERE host = $1;

-- name: GetCredentialStatus :one
-- Presence and validation state only, never the ciphertext itself --
-- backs CredentialService.GetCredentialStatus (docs/web-spec.md). has_token
-- is computed as token_ciphertext IS NOT NULL rather than read from a
-- separate stored flag, so it can never drift from whether a token is
-- actually present. The explicit ::boolean cast is load-bearing for sqlc's
-- pgx/v5 codegen: without it, sqlc cannot infer a concrete type for the
-- parenthesized boolean expression and emits `interface{}` for has_token
-- instead of `bool` (verified against sqlc v1.30.0 -- removing the cast
-- reproduces the interface{} field and a compile error at every call
-- site).
SELECT host, (token_ciphertext IS NOT NULL)::boolean AS has_token, validated FROM credentials WHERE host = $1;

-- name: ListCredentialStatuses :many
-- Every host's status, ordered by host -- backs
-- CredentialService.ListCredentials (docs/web-spec.md). Same has_token
-- computation and ::boolean cast as GetCredentialStatus.
SELECT host, (token_ciphertext IS NOT NULL)::boolean AS has_token, validated FROM credentials ORDER BY host;

-- name: SetCredentialValidated :one
-- Flips the validated flag set by the provider's ValidateToken result
-- (docs/web-spec.md "CredentialService"). Returns the same
-- presence/validation projection as GetCredentialStatus, not the
-- ciphertext, since callers of this path never need the plaintext. Same
-- ::boolean cast as GetCredentialStatus, for the same reason.
UPDATE credentials SET validated = $2, updated_at = now()
WHERE host = $1
RETURNING host, (token_ciphertext IS NOT NULL)::boolean AS has_token, validated;
