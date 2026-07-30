//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon; see
// main_integration_test.go's package doc for how to run this file (same
// package, same build tag -- it reuses startServer/newPostgres/
// adminRoundTripper/newIsolatedTransport from there and from
// registration_test.go).
//
// This file is loam-ofg.15's end-to-end proof for
// loam.admin.v1.CredentialService, against the REAL, booted binary with a
// REAL migrated Postgres and a REAL HTTP forge round trip. Nothing short
// of that catches the two things this bead is actually about:
//
//   - The token is genuinely encrypted at rest. A unit test can only
//     assert the handler called UpsertToken; only a live database can be
//     asked what bytes ended up in token_ciphertext.
//   - The token never reaches a log. The handler's own tests assert that
//     against an in-process buffer; this asserts it against the child
//     process's actual log stream, which is where an operator would find
//     it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
)

// The four tokens the stub Forgejo below distinguishes. Each is a
// distinctive literal so an "these bytes are absent" assertion cannot pass
// by coincidence, and so a leak found in a log is unambiguously
// attributable to the path that submitted it.
const (
	credValidToken       = "forgejo_valid-1f4b8c9e2a7d0356"
	credUnderscopedToken = "forgejo_underscoped-6b23e8f14c9a"
	credInvalidToken     = "forgejo_revoked-90d7a3f5e1c84b62"
	credEchoToken        = "forgejo_echo-canary-c5e79b02a4f16d83"
)

// stubForgejoAPI is the smallest server *forge.Forgejo's ValidateToken can
// be pointed at: it answers the scope probe
// (POST /api/v1/repos/<probe>/<probe>/pulls) with the status that maps to
// each sentinel -- 2xx accepted, 401 ErrInvalidToken, 403
// ErrInsufficientScope -- plus one deliberately hostile case.
//
// That hostile case is the point of the whole file's security half:
// credEchoToken gets a 500 whose BODY contains the submitted token,
// verbatim. This is exactly what a compromised, buggy, or merely chatty
// forge can do, and it is the only route by which a secret this process
// just handed out re-enters it inside attacker-influenced bytes. The 500
// takes the unclassified branch of the handler -- the one that
// deliberately PRESERVES detail for debugging and is therefore the one
// that would leak.
//
// The stub is a real HTTP server on 127.0.0.1 because the server under
// test is a separate OS process: an httptest handler mounted in-process
// would not be reachable from it. forge.apiBaseURL accepts a scheme-
// bearing host verbatim, so the stub's own URL is passed as the "forge
// host" for the credential.
func stubForgejoAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
		switch token {
		case credValidToken:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1})
		case credUnderscopedToken:
			http.Error(w, `{"message":"token does not have at least one of required scope(s): [write:repository]"}`, http.StatusForbidden)
		case credEchoToken:
			http.Error(w, fmt.Sprintf(`{"message":"internal server error while handling token %s"}`, token), http.StatusInternalServerError)
		default:
			http.Error(w, `{"message":"token does not exist"}`, http.StatusUnauthorized)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// serverLog is the child process's combined output, and specifically the
// stream internal/config's cfg.Logger writes to. That is STDOUT
// (config.go builds the JSON handler over os.Stdout), not stderr -- an
// assertion about log content made against stderr alone would read an
// empty buffer and pass without proving anything. Both are joined here so
// no future change to where a line is written can quietly hollow these
// assertions out.
func (rs *runningServer) serverLog() string {
	return rs.stdout.String() + rs.stderr.String()
}

func credentialClient(t *testing.T, rs *runningServer) adminv1connect.CredentialServiceClient {
	t.Helper()
	return adminv1connect.NewCredentialServiceClient(
		&http.Client{Transport: adminRoundTripper{user: testAdminUser, password: testAdminPassword, base: newIsolatedTransport(t)}},
		"http://"+rs.addr,
	)
}

// credentialCiphertext reads the raw token_ciphertext bytes the server
// stored for host, straight out of Postgres -- deliberately bypassing
// every Go type in this tree, because the question being asked is what is
// physically on disk, not what a store method reports.
func credentialCiphertext(t *testing.T, dsn, host string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { assert.NoError(t, conn.Close(context.Background())) }()
	var ciphertext []byte
	require.NoError(t, conn.QueryRow(ctx, "SELECT token_ciphertext FROM credentials WHERE host = $1", host).Scan(&ciphertext))
	return ciphertext
}

// TestServer_CredentialServiceIsRegistered_NotGroupFallback is
// loam-ofg.15's central registration proof. Before this bead,
// loam.admin.v1.CredentialService was declared in the proto and generated
// into internal/gen with no handler package and no registration anywhere,
// so every request fell through internal/server's group-level 404
// fallback -- which meant there was NO supported way to store an encrypted
// forge token at all, while EnrollRepo, internal/gittransport, and
// internal/mirrorsync had all been READING the credentials table since
// they landed (a plain psql INSERT cannot substitute: token_ciphertext is
// AES-GCM under LOAM_ENCRYPTION_KEY).
//
// Unlike its RepoAdmin/Proposal siblings, this proof needs no message
// forensics to discriminate the handler from the fallback: an
// unconfigured host is a SUCCESS here (features/credentials.feature,
// "Credentials are scoped per host" -> "it shows no credential is
// present"), and the fallback can only ever produce a 404.
func TestServer_CredentialServiceIsRegistered_NotGroupFallback(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	client := credentialClient(t, rs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.GetCredentialStatus(ctx, connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: "forgejo.example.com"}))
	require.NoError(t, err, "an unconfigured host must answer successfully; only the group fallback could turn this into an error")
	assert.Equal(t, "forgejo.example.com", resp.Msg.GetStatus().GetHost())
	assert.False(t, resp.Msg.GetStatus().GetHasToken(), "a freshly migrated database has no credentials for any host")
	assert.False(t, resp.Msg.GetStatus().GetValidated())
}

// TestServer_CredentialServiceRequiresAdmin proves the credential surface
// is unreachable without admin basic auth against the REAL binary:
// httpauth.Auth.AdminOnly answers 401 before any handler runs, so this
// never even reaches credential.requireAdmin (the handler-local defence in
// depth behind it).
func TestServer_CredentialServiceRequiresAdmin(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	client := adminv1connect.NewCredentialServiceClient(&http.Client{Transport: newIsolatedTransport(t)}, "http://"+rs.addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.SetUpstreamToken(ctx, connect.NewRequest(&adminv1.SetUpstreamTokenRequest{Host: "forgejo.example.com", Token: credValidToken}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeUnauthenticated, connectErr.Code())
	assert.NotContains(t, rs.serverLog(), credValidToken, "a token submitted by an unauthenticated caller must not be logged either")
}

// TestServer_SetUpstreamToken_EncryptsAtRestAndIsNeverReadableBack is the
// full round trip and this bead's central acceptance proof: the REAL
// binary validates a token against a REAL forge over HTTP, encrypts it
// with the AES-GCM key from LOAM_ENCRYPTION_KEY, stores it in a REAL
// Postgres, and reports it back as present and validated -- while the
// plaintext appears in NONE of the three responses, in no error, and in
// no line of the server's own stderr.
//
// The at-rest assertion reads token_ciphertext directly with pgx rather
// than through internal/credentialstore, because "the store's Encrypt was
// called" is a different (and much weaker) claim than "the bytes in the
// column are not the token".
func TestServer_SetUpstreamToken_EncryptsAtRestAndIsNeverReadableBack(t *testing.T) {
	dsn := newPostgres(t)
	rs := startServer(t, dsn)
	forgeStub := stubForgejoAPI(t)
	client := credentialClient(t, rs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setResp, err := client.SetUpstreamToken(ctx, connect.NewRequest(&adminv1.SetUpstreamTokenRequest{Host: forgeStub.URL, Token: credValidToken}))
	require.NoError(t, err)
	assert.Equal(t, forgeStub.URL, setResp.Msg.GetStatus().GetHost())
	assert.True(t, setResp.Msg.GetStatus().GetHasToken())
	assert.True(t, setResp.Msg.GetStatus().GetValidated(), "the forge accepted this token, so the stored verdict must say so")

	getResp, err := client.GetCredentialStatus(ctx, connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: forgeStub.URL}))
	require.NoError(t, err)
	assert.True(t, getResp.Msg.GetStatus().GetHasToken())
	assert.True(t, getResp.Msg.GetStatus().GetValidated())

	listResp, err := client.ListCredentials(ctx, connect.NewRequest(&adminv1.ListCredentialsRequest{}))
	require.NoError(t, err)
	require.Len(t, listResp.Msg.GetStatuses(), 1)
	assert.Equal(t, forgeStub.URL, listResp.Msg.GetStatuses()[0].GetHost())

	ciphertext := credentialCiphertext(t, dsn, forgeStub.URL)
	require.NotEmpty(t, ciphertext, "positive control: a token really was written to this row, so the assertion below is about real stored bytes")
	assert.False(t, bytes.Contains(ciphertext, []byte(credValidToken)), "token_ciphertext must not contain the plaintext token")

	for name, msg := range map[string]fmt.Stringer{
		"SetUpstreamToken":    setResp.Msg,
		"GetCredentialStatus": getResp.Msg,
		"ListCredentials":     listResp.Msg,
	} {
		assert.NotContains(t, msg.String(), credValidToken, name+" must not carry token material back to the caller")
	}
	assert.NotContains(t, rs.serverLog(), credValidToken, "the success path must not log the token")
}

// TestServer_SetUpstreamToken_BareHostAgainstPlaintextForge_Validates is
// the loam-4kz regression against the REAL binary: an admin (or a seeding
// script -- see Taskfile.yml's test:e2e target) submits a BARE
// "host:port", exactly what stubForgejoAPI's own doc comment calls a
// scheme-bearing form and this test deliberately strips, naming a real
// plaintext-HTTP server with no upstream URL anywhere in the request to
// borrow a scheme from. Before this fix, *forge.Forgejo's apiBaseURL
// dialled that bare host over https, the stub (plain HTTP, never
// NewTLSServer) answered with a plaintext HTTP response, and Go's client
// reported that decisively as http.ErrSchemeMismatch -- surfaced by the
// handler as an unclassified CodeInternal. ValidateToken now retries once
// over http on exactly that signal (internal/forge/forgejo.go), so this
// must now succeed, end to end, through the real server process.
func TestServer_SetUpstreamToken_BareHostAgainstPlaintextForge_Validates(t *testing.T) {
	dsn := newPostgres(t)
	rs := startServer(t, dsn)
	forgeStub := stubForgejoAPI(t)
	bareHost := strings.TrimPrefix(forgeStub.URL, "http://")
	require.NotEqual(t, forgeStub.URL, bareHost, "forgeStub must be a plain http:// httptest server for this to be a real regression test")
	client := credentialClient(t, rs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setResp, err := client.SetUpstreamToken(ctx, connect.NewRequest(&adminv1.SetUpstreamTokenRequest{Host: bareHost, Token: credValidToken}))
	require.NoError(t, err)
	assert.Equal(t, bareHost, setResp.Msg.GetStatus().GetHost(), "the stored key is the host exactly as submitted -- the scheme fallback is internal to validation, it never rewrites what credentials.host stores")
	assert.True(t, setResp.Msg.GetStatus().GetHasToken())
	assert.True(t, setResp.Msg.GetStatus().GetValidated(), "the forge accepted this token over the http fallback, so the stored verdict must say so")

	getResp, err := client.GetCredentialStatus(ctx, connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: bareHost}))
	require.NoError(t, err)
	assert.True(t, getResp.Msg.GetStatus().GetHasToken())
	assert.True(t, getResp.Msg.GetStatus().GetValidated())

	ciphertext := credentialCiphertext(t, dsn, bareHost)
	require.NotEmpty(t, ciphertext, "positive control: a token really was written under the bare host key")
	assert.False(t, bytes.Contains(ciphertext, []byte(credValidToken)), "token_ciphertext must not contain the plaintext token")
	assert.NotContains(t, rs.serverLog(), credValidToken, "the success path must not log the token")
}

// TestServer_SetUpstreamToken_RejectedTokenIsReportedAndNeverStored is
// features/credentials.feature's "A rejected token is reported", end to
// end. The store assertion is the load-bearing half: a handler that wrote
// first and errored afterwards would return the identical error, and would
// have destroyed whatever working token was previously on record.
func TestServer_SetUpstreamToken_RejectedTokenIsReportedAndNeverStored(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	forgeStub := stubForgejoAPI(t)
	client := credentialClient(t, rs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.SetUpstreamToken(ctx, connect.NewRequest(&adminv1.SetUpstreamTokenRequest{Host: forgeStub.URL, Token: credInvalidToken}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeInvalidArgument, connectErr.Code())
	assert.NotContains(t, connectErr.Message(), credInvalidToken)

	status, err := client.GetCredentialStatus(ctx, connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: forgeStub.URL}))
	require.NoError(t, err)
	assert.False(t, status.Msg.GetStatus().GetHasToken(), "a token the forge rejected must never have been written")
	assert.NotContains(t, rs.serverLog(), credInvalidToken)
}

// TestServer_SetUpstreamToken_UnderscopedTokenIsReportedDistinctly is the
// other forge sentinel, end to end: forge/errors.go requires this package
// to tell "does not authenticate" apart from "authenticates but lacks
// write:repository", and CredentialStatus has no field that could carry
// the difference, so the Connect code is where it has to show up.
func TestServer_SetUpstreamToken_UnderscopedTokenIsReportedDistinctly(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	forgeStub := stubForgejoAPI(t)
	client := credentialClient(t, rs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.SetUpstreamToken(ctx, connect.NewRequest(&adminv1.SetUpstreamTokenRequest{Host: forgeStub.URL, Token: credUnderscopedToken}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeFailedPrecondition, connectErr.Code(),
		"an underscoped token is not the same answer as an unauthenticated one (CodeInvalidArgument) or an unauthorized caller (CodePermissionDenied)")
	assert.Contains(t, connectErr.Message(), "write:repository")
	assert.NotContains(t, connectErr.Message(), credUnderscopedToken)
	assert.NotContains(t, rs.serverLog(), credUnderscopedToken)
}

// TestServer_SetUpstreamToken_ForgeEchoingTheTokenLeaksItNowhere is the
// security test this bead exists for, run against the real process. The
// stub forge replies 500 with the submitted token embedded in its body --
// the one path where bytes chosen by a third party carry a secret this
// process just handed out, and the one branch of the handler that
// deliberately preserves detail rather than discarding it.
//
// What this proves, precisely, is worth being exact about, because the
// answer turned out to be better than expected: with the CURRENT
// *forge.Forgejo, a non-2xx that is neither 401 nor 403 produces
// "unexpected status 500 Internal Server Error" and the response BODY is
// never read into the error at all -- so the echoed token does not even
// reach internal/handler/credential's redaction, let alone the log. This
// test pins that end-to-end property (a real forge, a real 500, a real
// hostile body, and the server's real log stream) rather than the
// redaction itself; the redaction's own positive control lives in
// internal/handler/credential's unit tests, where the validator seam can
// be made to return a hostile error directly and the [REDACTED] marker is
// asserted present. Both layers are needed: this one would keep passing if
// redaction were deleted TODAY, and the unit test would keep passing if
// forge.Forgejo started copying response bodies into its errors TOMORROW.
func TestServer_SetUpstreamToken_ForgeEchoingTheTokenLeaksItNowhere(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	forgeStub := stubForgejoAPI(t)
	client := credentialClient(t, rs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.SetUpstreamToken(ctx, connect.NewRequest(&adminv1.SetUpstreamTokenRequest{Host: forgeStub.URL, Token: credEchoToken}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeInternal, connectErr.Code())
	assert.NotContains(t, connectErr.Message(), credEchoToken)
	// The positive control is WAITED for, not sampled. handler.ErrorMapper
	// logs inside the child process, and the parent only sees it once the
	// child has flushed and this process has drained the stdout pipe --
	// neither of which is ordered against the RPC response arriving here.
	// Reading once immediately after the call turned CI red (run
	// 30374800718) with the captured log ending at "starting background
	// components": the assertion was racing the pipe, not observing a
	// missing line. This is loam-4q2's shape -- a single sample of
	// something that becomes true asynchronously.
	//
	// The absence assertions below then run against `logged`, the exact
	// content that satisfied the control, so they can never read a
	// narrower buffer than the one proven to contain the line.
	var logged string
	require.Eventuallyf(t, func() bool {
		logged = rs.serverLog()
		return strings.Contains(logged, "unmapped handler error") && strings.Contains(logged, forgeStub.URL)
	}, 30*time.Second, 50*time.Millisecond,
		"positive control: handler.ErrorMapper must really log this failure, and that line must be about THIS request -- otherwise the absence assertions below prove nothing. Last captured log: %s", logged)
	assert.NotContains(t, logged, credEchoToken, "a forge that echoes the submitted token must not put it in this server's log")
	assert.NotContains(t, logged, "internal server error while handling token",
		"no part of the forge's response body may reach the log -- the echoed token is only the worst thing it could contain")
}
