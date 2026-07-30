//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon; see
// main_integration_test.go's package doc for how to run this file (same
// package, same build tag -- it reuses newPostgres/migrateOnce/startServer/
// credentialClient/shortDataDir from there and from credential_integration_test.go).
//
// This file is loam-0ab's end-to-end proof: a REAL server, booted against a
// credentials row that a DIFFERENT LOAM_ENCRYPTION_KEY encrypted, must
// refuse to start rather than let CredentialService report a perfectly
// healthy credential that every real use (EnrollRepo, gittransport,
// mirrorsync) would then fail to decrypt.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"connectrpc.com/connect"

	"github.com/bobcob7/loam/internal/crypto"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
)

// credWrongKeyToken and credWrongKeyHost are the fixed plaintext token and
// host this file's tests use -- distinctive literals so an "absent from
// this output" assertion cannot pass by coincidence.
const (
	credWrongKeyToken = "forgejo_wrongkey-canary-a91fd0e6b3c8"
	credWrongKeyHost  = "forgejo.wrongkey.example.com"
)

// seedEncryptedCredential inserts one credentials row directly, encrypting
// token with a *crypto.Encryptor built from keyB64 -- exactly what
// internal/config.parseEncryptionKey decodes LOAM_ENCRYPTION_KEY into,
// replicated here rather than imported so this fixture does not depend on
// an unexported config function. This bypasses SetUpstreamToken/the forge
// validation round trip entirely: the point is to control precisely which
// key encrypted the row, not to exercise the RPC that normally writes it.
func seedEncryptedCredential(t *testing.T, dsn, keyB64, host, token string) {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(keyB64)
	require.NoError(t, err)
	enc, err := crypto.NewEncryptor(key)
	require.NoError(t, err)
	ciphertext, err := enc.Encrypt([]byte(token))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(ctx,
		`INSERT INTO credentials (id, host, token_ciphertext) VALUES ($1, $2, $3)`,
		uuid.Must(uuid.NewV7()), host, ciphertext,
	)
	require.NoError(t, err)
}

// randomBase64Key32 returns a fresh, random, base64-encoded 32-byte key --
// a "different key" that decodes cleanly (so config.Load's own length
// check passes) but was never used to encrypt anything.
func randomBase64Key32(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(key)
}

// TestServer_MatchingEncryptionKey_BootsAndReportsHealthy is the positive
// control for TestServer_WrongEncryptionKey_RefusesToStartInsteadOfReportingHealthy
// below: seeding + booting with the SAME key must succeed and the stored
// credential must be visible through CredentialService, or the sibling
// test's failure would prove nothing about the KEY being wrong specifically.
func TestServer_MatchingEncryptionKey_BootsAndReportsHealthy(t *testing.T) {
	dsn := newPostgres(t)
	migrateOnce(t, dsn)
	seedEncryptedCredential(t, dsn, testEncryptionKey, credWrongKeyHost, credWrongKeyToken)
	rs := startServer(t, dsn) // startServer always uses testEncryptionKey
	client := credentialClient(t, rs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.GetCredentialStatus(ctx, connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: credWrongKeyHost}))
	require.NoError(t, err, "stderr: %s", rs.stderr.String())
	assert.True(t, resp.Msg.GetStatus().GetHasToken(), "the seeded row must be visible once the booting key matches the one that encrypted it")
}

// TestServer_WrongEncryptionKey_RefusesToStartInsteadOfReportingHealthy is
// loam-0ab's central proof. Before this fix, a server booted with a
// mismatched LOAM_ENCRYPTION_KEY started cleanly and
// CredentialService.GetCredentialStatus/ListCredentials reported
// has_token=true for credWrongKeyHost regardless -- the admin's only health
// surface said everything was fine while every real use of that credential
// would fail to decrypt. This proves the fix instead: the process now
// refuses to start at all, loudly, naming the affected host, and never
// leaks the plaintext token or either encryption key into its own output.
//
// This test manually launches the binary (rather than through
// startServer/startServerWithEnv, both of which treat "never becomes
// ready" as a test failure via waitForReady's t.Fatalf) because becoming
// NOT ready is the entire point being proven here.
func TestServer_WrongEncryptionKey_RefusesToStartInsteadOfReportingHealthy(t *testing.T) {
	dsn := newPostgres(t)
	migrateOnce(t, dsn)
	seedEncryptedCredential(t, dsn, testEncryptionKey, credWrongKeyHost, credWrongKeyToken)
	wrongKey := randomBase64Key32(t)
	require.NotEqual(t, testEncryptionKey, wrongKey, "the whole point is a DIFFERENT key from the one that encrypted the row")

	cmd := exec.Command(serverBinary)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"LOAM_HTTP_ADDR=127.0.0.1:0",
		"LOAM_ADMIN_USER=" + testAdminUser,
		"LOAM_ADMIN_PASSWORD=" + testAdminPassword,
		"LOAM_DATABASE_URL=" + dsn,
		"LOAM_ENCRYPTION_KEY=" + wrongKey,
		"LOAM_DATA_DIR=" + shortDataDir(t),
	}
	var output syncBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		require.Error(t, err, "a server booted against a credential it cannot decrypt must exit non-zero, not start cleanly; output: %s", output.String())
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("server neither started nor exited within the deadline; output: %s", output.String())
	}
	require.NotNil(t, cmd.ProcessState)
	assert.Equal(t, 1, cmd.ProcessState.ExitCode(), "output: %s", output.String())

	logged := output.String()
	assert.Contains(t, logged, credWrongKeyHost,
		"the operator-facing failure should name which host's credential could not be decrypted")
	assert.NotContains(t, logged, credWrongKeyToken,
		"the boot failure must never leak the plaintext token it could not recover")
	assert.NotContains(t, logged, wrongKey,
		"the boot failure must never leak the (wrong) key bytes")
	assert.NotContains(t, logged, testEncryptionKey,
		"the boot failure must never leak the (right) key bytes either")
	assert.False(t, strings.Contains(logged, "panic:"),
		"a mismatched encryption key is a configuration error, never a panic")
}
