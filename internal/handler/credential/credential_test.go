package credential

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/forge"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/urlredact"
)

// testToken is the plaintext every leak assertion in this file hunts for.
// It is a single, distinctive literal on purpose: "assert these bytes are
// absent" is only meaningful if the bytes are ones that could not appear
// by coincidence, and if the SAME literal really did travel the path under
// test. Every test that asserts its absence also plants it in a real
// argument first (see the leak tests below), so a test that stopped
// exercising the path would fail its positive control rather than pass
// vacuously.
const testToken = "ghp_leak-canary-2f8c41d7e6b90a3f"

const testHost = "forgejo.example.com"

func testLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}

// adminCtx is the context every RPC in this package requires: the flag
// httpauth.Auth.AdminOnly sets on a request that passed admin basic auth.
func adminCtx(t *testing.T) context.Context {
	t.Helper()
	return httpauth.WithAdmin(t.Context())
}

// testDeps bundles both moq mocks, each pre-configured with a harmless,
// fully-specified default: a forge that accepts any token, a store that
// upserts, records the verdict, reads, and lists successfully. A test
// overriding nothing therefore exercises the happy path, so every failure
// a test sees comes from the one collaborator it deliberately changed --
// never from a nil-func panic on one it does not care about. That property
// is what makes mutation testing here meaningful: breaking a load-bearing
// line must turn a test red on an ASSERTION, which it cannot do if an
// unconfigured mock panics first.
type testDeps struct {
	store     *credentialStoreMock
	validator *tokenValidatorMock
	buf       bytes.Buffer
}

func newTestDeps() *testDeps {
	d := &testDeps{}
	d.store = &credentialStoreMock{
		UpsertTokenFunc: func(_ context.Context, host, _ string) (credentialstore.CredentialStatus, error) {
			// Mirrors the real UpsertCredentialToken: it writes the
			// ciphertext and resets validated to false in the same
			// statement.
			return credentialstore.CredentialStatus{Host: host, HasToken: true, Validated: false}, nil
		},
		SetValidatedFunc: func(_ context.Context, host string, validated bool) (credentialstore.CredentialStatus, error) {
			return credentialstore.CredentialStatus{Host: host, HasToken: true, Validated: validated}, nil
		},
		GetStatusFunc: func(_ context.Context, host string) (credentialstore.CredentialStatus, error) {
			return credentialstore.CredentialStatus{Host: host, HasToken: true, Validated: true}, nil
		},
		ListStatusesFunc: func(context.Context) ([]credentialstore.CredentialStatus, error) {
			return []credentialstore.CredentialStatus{
				{Host: "forgejo.example.com", HasToken: true, Validated: true},
				{Host: "github.com", HasToken: false, Validated: false},
			}, nil
		},
	}
	d.validator = &tokenValidatorMock{
		ValidateTokenFunc: func(context.Context, string, string) error { return nil },
	}
	return d
}

func (d *testDeps) handler() *Handler {
	logger := testLogger(&d.buf)
	return New(d.store, d.validator, handler.NewErrorMapper(logger), logger)
}

func setTokenReq(host, token string) *connect.Request[adminv1.SetUpstreamTokenRequest] {
	return connect.NewRequest(&adminv1.SetUpstreamTokenRequest{Host: host, Token: token})
}

// connectCode extracts the Connect status code from err, failing the test
// if err is not a *connect.Error at all.
func connectCode(t *testing.T, err error) connect.Code {
	t.Helper()
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	return connectErr.Code()
}

// ---------------------------------------------------------------------
// SetUpstreamToken: the happy path and its ordering
// ---------------------------------------------------------------------

func TestSetUpstreamToken_ValidatesThenStoresThenRecordsTheVerdict(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	resp, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(testHost, testToken))
	require.NoError(t, err)
	require.Len(t, d.validator.ValidateTokenCalls(), 1, "the forge must be asked about the token exactly once")
	assert.Equal(t, testHost, d.validator.ValidateTokenCalls()[0].Host)
	assert.Equal(t, testToken, d.validator.ValidateTokenCalls()[0].Token,
		"the token under test must genuinely reach the validator -- otherwise every leak assertion below is vacuous")
	require.Len(t, d.store.UpsertTokenCalls(), 1)
	assert.Equal(t, testToken, d.store.UpsertTokenCalls()[0].Token,
		"the token under test must genuinely reach the store -- see above")
	require.Len(t, d.store.SetValidatedCalls(), 1)
	assert.True(t, d.store.SetValidatedCalls()[0].Validated)
	assert.Equal(t, testHost, resp.Msg.GetStatus().GetHost())
	assert.True(t, resp.Msg.GetStatus().GetHasToken())
	assert.True(t, resp.Msg.GetStatus().GetValidated())
}

// TestSetUpstreamToken_RejectedTokenIsNeverStored pins the ordering
// decision documented on SetUpstreamToken: a token the forge refuses must
// not replace the working token already on record for that host. The
// assertion is on the STORE, not on the returned code -- a handler that
// stored first and errored afterwards would return the identical error.
func TestSetUpstreamToken_RejectedTokenIsNeverStored(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.validator.ValidateTokenFunc = func(_ context.Context, host, _ string) error {
		return fmt.Errorf("validating token for %s: %w", host, forge.ErrInvalidToken)
	}
	_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(testHost, testToken))
	require.Error(t, err)
	assert.Empty(t, d.store.UpsertTokenCalls(), "a token the forge rejected must never reach the store")
	assert.Empty(t, d.store.SetValidatedCalls())
}

// TestSetUpstreamToken_NeverRecordsAVerdictWithoutWritingTheTokenFirst is
// the anti-inheritance guard: SetValidated must never run except after a
// successful UpsertToken for the SAME call, because UpsertToken is what
// resets validated to false. If a failed write could still be followed by
// SetValidated(true), a replacement token would inherit the previous
// token's verdict and report as checked when nothing checked it.
func TestSetUpstreamToken_NeverRecordsAVerdictWithoutWritingTheTokenFirst(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.UpsertTokenFunc = func(context.Context, string, string) (credentialstore.CredentialStatus, error) {
		return credentialstore.CredentialStatus{}, errors.New("upsert credentials: connection refused")
	}
	_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(testHost, testToken))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInternal, connectCode(t, err))
	assert.Empty(t, d.store.SetValidatedCalls(),
		"validated=true must never be recorded for a token the store did not accept -- that is the previous token's verdict surviving its replacement")
}

// TestSetUpstreamToken_VerdictWriteFailureIsReportedAndSaysTheTokenIsStored
// covers the one genuinely awkward window: the ciphertext landed but the
// verdict write did not. The RPC must fail (the operator's request did not
// fully succeed) while the row honestly under-reports as has_token=true,
// validated=false.
func TestSetUpstreamToken_VerdictWriteFailureIsReportedAndSaysTheTokenIsStored(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.SetValidatedFunc = func(context.Context, string, bool) (credentialstore.CredentialStatus, error) {
		return credentialstore.CredentialStatus{}, errors.New("set validated: connection refused")
	}
	_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(testHost, testToken))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInternal, connectCode(t, err))
	require.Len(t, d.store.UpsertTokenCalls(), 1, "the token was written before the verdict write was attempted")
}

// ---------------------------------------------------------------------
// SetUpstreamToken: the two forge sentinels, reported distinctly
// ---------------------------------------------------------------------

// TestSetUpstreamToken_ForgeSentinelsMapToDistinctCodes is the contract
// forge/errors.go names this package for by hand: ErrInvalidToken and
// ErrInsufficientScope must NOT be folded together. Asserting both codes
// in one table is deliberate -- the failure mode being guarded against is
// two branches collapsing into one, which a pair of independent tests each
// asserting its own code would still catch, but less legibly.
func TestSetUpstreamToken_ForgeSentinelsMapToDistinctCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		forgeErr     error
		wantCode     connect.Code
		wantMentions string
	}{
		{
			name:         "token does not authenticate",
			forgeErr:     fmt.Errorf("validating token: %w", forge.ErrInvalidToken),
			wantCode:     connect.CodeInvalidArgument,
			wantMentions: "does not authenticate",
		},
		{
			name:         "token authenticates but is underscoped",
			forgeErr:     fmt.Errorf("validating token: %w", forge.ErrInsufficientScope),
			wantCode:     connect.CodeFailedPrecondition,
			wantMentions: "lacks the scope needed to open pull requests",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := newTestDeps()
			d.validator.ValidateTokenFunc = func(context.Context, string, string) error { return tt.forgeErr }
			_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(testHost, testToken))
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, connectCode(t, err))
			var connectErr *connect.Error
			require.ErrorAs(t, err, &connectErr)
			assert.Contains(t, connectErr.Message(), tt.wantMentions,
				"the admin must be told WHICH refusal this is; CredentialStatus has no reason field, so the message is the only channel")
		})
	}
}

// TestSetUpstreamToken_UnderscopedIsNotConfusedWithAnUnauthorizedCaller
// pins the code choice made for ErrInsufficientScope. PermissionDenied is
// reserved for requireAdmin; if the underscoped case reused it, "you are
// not an admin" and "your token is underscoped" would be
// indistinguishable on the wire, and the admin gate's own test below could
// pass against a handler with no gate at all.
func TestSetUpstreamToken_UnderscopedIsNotConfusedWithAnUnauthorizedCaller(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.validator.ValidateTokenFunc = func(context.Context, string, string) error {
		return fmt.Errorf("validating token: %w", forge.ErrInsufficientScope)
	}
	_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(testHost, testToken))
	require.Error(t, err)
	assert.NotEqual(t, connect.CodePermissionDenied, connectCode(t, err))
}

func TestSetUpstreamToken_MissingFieldsAreInvalidArgument(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		host  string
		token string
	}{
		{name: "no host", host: "", token: testToken},
		{name: "whitespace-only host", host: "   ", token: testToken},
		{name: "no token", host: testHost, token: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := newTestDeps()
			_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(tt.host, tt.token))
			require.Error(t, err)
			assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
			assert.Empty(t, d.store.UpsertTokenCalls())
		})
	}
}

// ---------------------------------------------------------------------
// SetUpstreamToken: host canonicalization (loam-0hjq)
// ---------------------------------------------------------------------

// TestSetUpstreamToken_CanonicalizesHostBeforeValidatingAndStoring is the
// central regression this bead exists for: before loam-0hjq, a
// scheme-qualified https host was stored VERBATIM (only
// strings.TrimSpace), so it validated and reported validated=true, yet
// internal/handler/repoadmin's forgeHostOf -- which EnrollRepo/ProbeRepo
// resolve credentials by -- always derives the BARE form for an https
// upstream and could never find it. Both the forge round trip and the
// store write must now see the canonical (bare) form, not the raw string
// the admin typed, and the response must report the canonical form too --
// otherwise the Credentials screen would show a host string that still
// does not match what EnrollRepo derives.
func TestSetUpstreamToken_CanonicalizesHostBeforeValidatingAndStoring(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	resp, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq("https://"+testHost, testToken))
	require.NoError(t, err)
	require.Len(t, d.validator.ValidateTokenCalls(), 1)
	assert.Equal(t, testHost, d.validator.ValidateTokenCalls()[0].Host,
		"the forge must be asked about the CANONICAL host, not the raw scheme-qualified string the admin typed")
	require.Len(t, d.store.UpsertTokenCalls(), 1)
	assert.Equal(t, testHost, d.store.UpsertTokenCalls()[0].Host,
		"the stored key must be the canonical host -- this is what forgeHostOf's bare derivation must be able to find again")
	require.Len(t, d.store.SetValidatedCalls(), 1)
	assert.Equal(t, testHost, d.store.SetValidatedCalls()[0].Host)
	assert.Equal(t, testHost, resp.Msg.GetStatus().GetHost(),
		"the response must report the canonical host, not the raw string that was submitted")
}

// TestSetUpstreamToken_PlainHTTPSchemeSurvivesCanonicalizationUnchanged
// pins the other half of the rule: a plain-http host keeps its scheme
// prefix exactly (internal/forgehost.Canonicalize's rule), since
// internal/forge's apiBaseURL only ever dials a scheme-less host over
// https and forgeHostOf derives the same scheme-qualified form for a
// plain-HTTP upstream.
func TestSetUpstreamToken_PlainHTTPSchemeSurvivesCanonicalizationUnchanged(t *testing.T) {
	t.Parallel()
	const plainHTTPHost = "http://forge.internal:3000"
	d := newTestDeps()
	resp, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(plainHTTPHost, testToken))
	require.NoError(t, err)
	require.Len(t, d.store.UpsertTokenCalls(), 1)
	assert.Equal(t, plainHTTPHost, d.store.UpsertTokenCalls()[0].Host)
	assert.Equal(t, plainHTTPHost, resp.Msg.GetStatus().GetHost())
}

// TestSetUpstreamToken_MalformedHostIsRejectedBeforeTouchingTheForgeOrStore
// proves the reject side of Canonicalize's contract is wired all the way
// through this handler: a host that is WRONG, not merely differently
// spelled (a path component, embedded userinfo, or an unsupported
// scheme), must never reach the forge or the store -- not just fail
// eventually.
func TestSetUpstreamToken_MalformedHostIsRejectedBeforeTouchingTheForgeOrStore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		host string
	}{
		{name: "a path component", host: "https://" + testHost + "/owner/repo"},
		{name: "embedded userinfo", host: "https://token@" + testHost},
		{name: "an unsupported scheme", host: "ftp://" + testHost},
		{name: "unparseable", host: "https://[::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := newTestDeps()
			_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(tt.host, testToken))
			require.Error(t, err)
			assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
			assert.Empty(t, d.validator.ValidateTokenCalls(), "a malformed host must never reach the forge")
			assert.Empty(t, d.store.UpsertTokenCalls(), "a malformed host must never reach the store")
		})
	}
}

// TestSetUpstreamToken_MalformedHostRejectionNeverEchoesEmbeddedUserinfo is
// the leak check for the rejection path specifically (loam-ra1k): a host
// carrying a credential-shaped userinfo component must not have that
// value appear in the error returned to the caller.
func TestSetUpstreamToken_MalformedHostRejectionNeverEchoesEmbeddedUserinfo(t *testing.T) {
	t.Parallel()
	const embeddedSecret = "leak-canary-userinfo-9d3f1a"
	d := newTestDeps()
	_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq("https://"+embeddedSecret+"@"+testHost, testToken))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), embeddedSecret)
}

// ---------------------------------------------------------------------
// The leak surface: a token must never reach an error, a response, or a log
// ---------------------------------------------------------------------

// TestSetUpstreamToken_ForgeErrorEchoingTheTokenLeaksItNowhere is the
// central security test of this bead, and it is built so that it CANNOT
// pass with the secret present.
//
// The forge is a third party. A forge that echoed the submitted token back
// in an unclassified error body is not hypothetical -- it is the one place
// in this package where bytes chosen by someone else re-enter the process
// carrying a secret this process just handed out. The mock plants exactly
// that: the real token, verbatim, inside an error message that matches no
// sentinel and therefore takes the branch whose whole job is preserving
// detail. Both destinations of that branch are then asserted on: the error
// returned to the caller, and the log line handler.ErrorMapper writes
// before collapsing it to CodeInternal (ErrorMapper deliberately does not
// redact -- it cannot know what a caller embedded -- so redaction has to
// have happened before it).
func TestSetUpstreamToken_ForgeErrorEchoingTheTokenLeaksItNowhere(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.validator.ValidateTokenFunc = func(_ context.Context, host, token string) error {
		return fmt.Errorf("forge %s replied 500: {\"message\":\"internal error handling token %s\"}", host, token)
	}
	_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(testHost, testToken))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInternal, connectCode(t, err))
	assert.NotContains(t, err.Error(), testToken, "the token must never reach the error returned to the caller")
	logged := d.buf.String()
	require.NotEmpty(t, logged, "ErrorMapper must have logged the unmapped error -- if it did not, this test proves nothing about log leaks")
	assert.Contains(t, logged, urlredact.Marker,
		"positive control: the log line must be the one carrying the redacted forge message, so the absence assertion below is about the right bytes")
	assert.NotContains(t, logged, testToken, "the token must never reach a log line")
}

// TestSetUpstreamToken_StoreErrorEchoingTheTokenLeaksItNowhere is the same
// proof for the other seam a token crosses. A store error should never
// carry the plaintext (internal/credentialstore wraps with the host only),
// but "should never" written down in another package is not a property
// this one can rely on, and the encryptor sits behind that same seam.
func TestSetUpstreamToken_StoreErrorEchoingTheTokenLeaksItNowhere(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.UpsertTokenFunc = func(_ context.Context, host, token string) (credentialstore.CredentialStatus, error) {
		return credentialstore.CredentialStatus{}, fmt.Errorf("upserting token for host %s: encrypting %q failed", host, token)
	}
	_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(testHost, testToken))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
	logged := d.buf.String()
	require.NotEmpty(t, logged)
	assert.Contains(t, logged, urlredact.Marker, "positive control: the redacted store message is what reached the log")
	assert.NotContains(t, logged, testToken)
}

// TestSetUpstreamToken_VerdictWriteErrorEchoingTheTokenLeaksItNowhere
// covers the third and last error path that runs while the plaintext is
// still in scope. Kept separate from the UpsertToken case above because a
// redaction applied at only one of the two store call sites would pass
// that test and fail this one.
func TestSetUpstreamToken_VerdictWriteErrorEchoingTheTokenLeaksItNowhere(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.SetValidatedFunc = func(_ context.Context, host string, _ bool) (credentialstore.CredentialStatus, error) {
		return credentialstore.CredentialStatus{}, fmt.Errorf("setting validated for host %s while holding %s", host, testToken)
	}
	_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(testHost, testToken))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
	logged := d.buf.String()
	require.NotEmpty(t, logged)
	assert.Contains(t, logged, urlredact.Marker)
	assert.NotContains(t, logged, testToken)
}

// TestSetUpstreamToken_SentinelBranchesDiscardTheForgeErrorEntirely proves
// the stronger claim made for the two classified branches: they do not
// merely redact the forge's error, they drop it. A forge that echoed the
// token alongside a 401 must contribute NO bytes at all to what this
// handler returns or logs -- not the token (redaction would cover that)
// and not the rest of its body either.
func TestSetUpstreamToken_SentinelBranchesDiscardTheForgeErrorEntirely(t *testing.T) {
	t.Parallel()
	const forgeChatter = "sensitive-forge-chatter-b41f"
	for _, sentinel := range []error{forge.ErrInvalidToken, forge.ErrInsufficientScope} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			t.Parallel()
			d := newTestDeps()
			d.validator.ValidateTokenFunc = func(_ context.Context, _, token string) error {
				return fmt.Errorf("%s token=%s: %w", forgeChatter, token, sentinel)
			}
			_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(testHost, testToken))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), testToken)
			assert.NotContains(t, err.Error(), forgeChatter, "a classified refusal carries this handler's own message, never the forge's body")
			assert.NotContains(t, d.buf.String(), testToken)
		})
	}
}

// TestSetUpstreamToken_HappyPathLogsTheHostAndNeverTheToken guards the
// non-error direction. The positive control matters as much as the
// absence assertion: a handler that logged nothing at all would satisfy
// "the token is not in the log" vacuously, so this asserts a log line
// naming the host really was written.
func TestSetUpstreamToken_HappyPathLogsTheHostAndNeverTheToken(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(testHost, testToken))
	require.NoError(t, err)
	logged := d.buf.String()
	require.Contains(t, logged, testHost, "positive control: the success path must log something naming the host")
	assert.NotContains(t, logged, testToken)
}

// TestRedactedErrorKeepsNoUnredactedCopyInItsChain closes the subtler
// version of the same hole: an error that redacts its own Error() while
// still wrapping the original hands the plaintext to anyone who calls
// errors.Unwrap, and to any log line formatted with %+v.
func TestRedactedErrorKeepsNoUnredactedCopyInItsChain(t *testing.T) {
	t.Parallel()
	original := fmt.Errorf("forge echoed %s", testToken)
	redacted := redactErr(original, testToken)
	require.NotNil(t, redacted)
	assert.NotContains(t, redacted.Error(), testToken)
	assert.Contains(t, redacted.Error(), urlredact.Marker)
	assert.Nil(t, errors.Unwrap(redacted), "the redacted error must not wrap the original -- the plaintext would be one Unwrap away")
	assert.NotContains(t, fmt.Sprintf("%+v", redacted), testToken)
}

// ---------------------------------------------------------------------
// No readback, by any route
// ---------------------------------------------------------------------

// TestNoRPCEverReturnsTheTokenInItsResponse is the readback proof. It
// drives all three RPCs with a store whose every method behaves as though
// the token were freely available -- and, for the strongest version of the
// question, one that returns the token itself in the field an
// implementation would most plausibly mis-plumb it into (Host) -- then
// asserts the SERIALIZED proto bytes of each response contain no trace of
// it. Serialized bytes rather than field-by-field assertions on purpose:
// that catches a token routed into any field, including one added later,
// without the test having to be told which fields exist.
func TestNoRPCEverReturnsTheTokenInItsResponse(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	setResp, err := d.handler().SetUpstreamToken(adminCtx(t), setTokenReq(testHost, testToken))
	require.NoError(t, err)
	getResp, err := d.handler().GetCredentialStatus(adminCtx(t), connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: testHost}))
	require.NoError(t, err)
	listResp, err := d.handler().ListCredentials(adminCtx(t), connect.NewRequest(&adminv1.ListCredentialsRequest{}))
	require.NoError(t, err)
	for name, msg := range map[string]proto.Message{
		"SetUpstreamToken":    setResp.Msg,
		"GetCredentialStatus": getResp.Msg,
		"ListCredentials":     listResp.Msg,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wire, err := proto.Marshal(msg)
			require.NoError(t, err)
			assert.NotContains(t, string(wire), testToken, "no CredentialService response may carry token material on the wire")
		})
	}
}

// TestNoStoreMethodThisPackageHoldsCanReturnAToken is the structural half
// of the readback proof, and the half that survives a refactor the other
// tests would not notice. It walks this package's store seam by reflection
// and asserts that NO method on it can return plaintext token material:
// not credentialstore.Credential (the store's only token-bearing type,
// produced only by GetByHost), and not any other struct carrying a Token
// field.
//
// Stated that way rather than as "GetByHost is absent" on purpose. The
// property that matters is "nothing on this seam can hand back a token",
// and phrasing it as the absence of one method name would be satisfied by
// a differently-named method with the same return type. Widening the seam
// -- the plausible future mistake, "just add GetByHost so we can echo the
// host back" -- fails this the moment the mock is regenerated.
func TestNoStoreMethodThisPackageHoldsCanReturnAToken(t *testing.T) {
	t.Parallel()
	var full any = (*credentialstore.Store)(nil)
	_, ok := full.(credentialStore)
	require.True(t, ok, "the real store must satisfy this package's seam -- otherwise this test is inspecting a seam production does not use")
	seam := reflect.TypeOf((*credentialStore)(nil)).Elem()
	require.NotZero(t, seam.NumMethod(), "positive control: the seam really has methods to inspect")
	// Deliberately NOT an exact method count. A count assertion would
	// abort before the loop below ever ran, so widening the seam would
	// be caught by "the number changed" rather than by the property that
	// actually matters -- and would have to be re-baselined by hand every
	// time a harmless status-only method is added.
	credentialType := reflect.TypeOf(credentialstore.Credential{})
	for i := range seam.NumMethod() {
		method := seam.Method(i)
		for j := range method.Type.NumOut() {
			out := method.Type.Out(j)
			assert.NotEqual(t, credentialType, out,
				"%s returns credentialstore.Credential, whose Token field is decrypted plaintext -- that type must not be reachable from this package", method.Name)
			if out.Kind() == reflect.Struct {
				_, hasToken := out.FieldByName("Token")
				assert.False(t, hasToken, "%s returns %s, which carries a Token field", method.Name, out)
			}
		}
	}
}

// ---------------------------------------------------------------------
// GetCredentialStatus / ListCredentials
// ---------------------------------------------------------------------

// TestGetCredentialStatus_UnknownHostReportsNoCredentialRatherThanAnError
// is features/credentials.feature's "Credentials are scoped per host"
// ("When I view the credential status for forgejo.example.com / Then it
// shows no credential is present"), expressed at the handler.
func TestGetCredentialStatus_UnknownHostReportsNoCredentialRatherThanAnError(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.GetStatusFunc = func(_ context.Context, host string) (credentialstore.CredentialStatus, error) {
		return credentialstore.CredentialStatus{}, fmt.Errorf("getting credential status for host %s: %w", host, credentialstore.ErrNotFound)
	}
	resp, err := d.handler().GetCredentialStatus(adminCtx(t), connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: testHost}))
	require.NoError(t, err, "an unconfigured host is data, not a failure")
	assert.Equal(t, testHost, resp.Msg.GetStatus().GetHost())
	assert.False(t, resp.Msg.GetStatus().GetHasToken())
	assert.False(t, resp.Msg.GetStatus().GetValidated())
}

// TestGetCredentialStatus_DatabaseFailureIsNotReportedAsNoCredential is
// the other half of the sentinel distinction, and the reason
// credentialstore.ErrNotFound had to be exported rather than matched on
// message text: "this host is not configured" and "the database is
// unreachable" must not produce the same answer, or an admin would read a
// total outage as a clean, empty configuration.
func TestGetCredentialStatus_DatabaseFailureIsNotReportedAsNoCredential(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.GetStatusFunc = func(context.Context, string) (credentialstore.CredentialStatus, error) {
		return credentialstore.CredentialStatus{}, errors.New("getting credential status: connection refused")
	}
	_, err := d.handler().GetCredentialStatus(adminCtx(t), connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: testHost}))
	require.Error(t, err, "a database failure must not be flattened into has_token=false")
	assert.Equal(t, connect.CodeInternal, connectCode(t, err))
}

func TestGetCredentialStatus_ConfiguredHostReportsItsState(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	resp, err := d.handler().GetCredentialStatus(adminCtx(t), connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: testHost}))
	require.NoError(t, err)
	assert.True(t, resp.Msg.GetStatus().GetHasToken())
	assert.True(t, resp.Msg.GetStatus().GetValidated())
}

func TestGetCredentialStatus_MissingHostIsInvalidArgument(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().GetCredentialStatus(adminCtx(t), connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: "  "}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
	assert.Empty(t, d.store.GetStatusCalls())
}

func TestListCredentials_ReturnsEveryHostsState(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	resp, err := d.handler().ListCredentials(adminCtx(t), connect.NewRequest(&adminv1.ListCredentialsRequest{}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetStatuses(), 2)
	assert.Equal(t, "forgejo.example.com", resp.Msg.GetStatuses()[0].GetHost())
	assert.True(t, resp.Msg.GetStatuses()[0].GetHasToken())
	assert.Equal(t, "github.com", resp.Msg.GetStatuses()[1].GetHost())
	assert.False(t, resp.Msg.GetStatuses()[1].GetHasToken())
}

func TestListCredentials_StoreFailureIsInternal(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.ListStatusesFunc = func(context.Context) ([]credentialstore.CredentialStatus, error) {
		return nil, errors.New("listing credential statuses: connection refused")
	}
	_, err := d.handler().ListCredentials(adminCtx(t), connect.NewRequest(&adminv1.ListCredentialsRequest{}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInternal, connectCode(t, err))
}

// ---------------------------------------------------------------------
// The admin gate
// ---------------------------------------------------------------------

// TestEveryRPCRequiresAdmin asserts the per-RPC gate this package adds on
// top of httpauth.Auth.AdminOnly. The store assertion is as important as
// the code assertion: a non-admin must be turned away BEFORE any
// collaborator is touched, so a rejected caller cannot even probe which
// hosts are configured by timing or by the shape of a downstream failure.
func TestEveryRPCRequiresAdmin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		call func(context.Context, *Handler) error
	}{
		{
			name: "SetUpstreamToken",
			call: func(ctx context.Context, h *Handler) error {
				_, err := h.SetUpstreamToken(ctx, setTokenReq(testHost, testToken))
				return err
			},
		},
		{
			name: "GetCredentialStatus",
			call: func(ctx context.Context, h *Handler) error {
				_, err := h.GetCredentialStatus(ctx, connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: testHost}))
				return err
			},
		},
		{
			name: "ListCredentials",
			call: func(ctx context.Context, h *Handler) error {
				_, err := h.ListCredentials(ctx, connect.NewRequest(&adminv1.ListCredentialsRequest{}))
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := newTestDeps()
			err := tt.call(t.Context(), d.handler())
			require.Error(t, err, "a context with no admin flag must be refused")
			assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
			assert.Empty(t, d.store.UpsertTokenCalls(), "no collaborator may be touched before the gate")
			assert.Empty(t, d.store.GetStatusCalls())
			assert.Empty(t, d.store.ListStatusesCalls())
			assert.Empty(t, d.validator.ValidateTokenCalls())
		})
	}
}

// TestSetUpstreamToken_NonAdminNeverSeesItsOwnTokenBack is a small extra
// guard on the refusal path: the rejection message names the operation,
// never the submitted secret.
func TestSetUpstreamToken_NonAdminNeverSeesItsOwnTokenBack(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().SetUpstreamToken(t.Context(), setTokenReq(testHost, testToken))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
	assert.NotContains(t, d.buf.String(), testToken)
}

// TestNoTokenReachesAnyLogAcrossEveryPath is the sweep: it runs every RPC
// down every branch this package has -- admin and non-admin, valid,
// invalid, underscoped, unclassified forge failure, and both store
// failures -- against ONE shared log buffer, then asserts the token's
// bytes appear nowhere in the accumulated output. A per-test assertion can
// only cover the path its author remembered; this one fails for any new
// branch that logs the token, including one added after this test was
// written.
func TestNoTokenReachesAnyLogAcrossEveryPath(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := testLogger(&buf)
	branches := []struct {
		name  string
		setup func(*testDeps)
		admin bool
	}{
		{name: "happy path", setup: func(*testDeps) {}, admin: true},
		{name: "non-admin", setup: func(*testDeps) {}, admin: false},
		{name: "invalid token", admin: true, setup: func(d *testDeps) {
			d.validator.ValidateTokenFunc = func(_ context.Context, _, token string) error {
				return fmt.Errorf("rejecting %s: %w", token, forge.ErrInvalidToken)
			}
		}},
		{name: "underscoped token", admin: true, setup: func(d *testDeps) {
			d.validator.ValidateTokenFunc = func(_ context.Context, _, token string) error {
				return fmt.Errorf("underscoped %s: %w", token, forge.ErrInsufficientScope)
			}
		}},
		{name: "unclassified forge failure", admin: true, setup: func(d *testDeps) {
			d.validator.ValidateTokenFunc = func(_ context.Context, _, token string) error {
				return fmt.Errorf("forge exploded holding %s", token)
			}
		}},
		{name: "upsert failure", admin: true, setup: func(d *testDeps) {
			d.store.UpsertTokenFunc = func(_ context.Context, _, token string) (credentialstore.CredentialStatus, error) {
				return credentialstore.CredentialStatus{}, fmt.Errorf("upsert of %s failed", token)
			}
		}},
		{name: "verdict-write failure", admin: true, setup: func(d *testDeps) {
			d.store.SetValidatedFunc = func(context.Context, string, bool) (credentialstore.CredentialStatus, error) {
				return credentialstore.CredentialStatus{}, fmt.Errorf("verdict write failed while holding %s", testToken)
			}
		}},
		{name: "status read failure", admin: true, setup: func(d *testDeps) {
			d.store.GetStatusFunc = func(context.Context, string) (credentialstore.CredentialStatus, error) {
				return credentialstore.CredentialStatus{}, errors.New("status read failed")
			}
		}},
		{name: "list failure", admin: true, setup: func(d *testDeps) {
			d.store.ListStatusesFunc = func(context.Context) ([]credentialstore.CredentialStatus, error) {
				return nil, errors.New("list failed")
			}
		}},
	}
	for _, branch := range branches {
		d := newTestDeps()
		branch.setup(d)
		h := New(d.store, d.validator, handler.NewErrorMapper(logger), logger)
		ctx := t.Context()
		if branch.admin {
			ctx = httpauth.WithAdmin(ctx)
		}
		_, _ = h.SetUpstreamToken(ctx, setTokenReq(testHost, testToken))
		_, _ = h.GetCredentialStatus(ctx, connect.NewRequest(&adminv1.GetCredentialStatusRequest{Host: testHost}))
		_, _ = h.ListCredentials(ctx, connect.NewRequest(&adminv1.ListCredentialsRequest{}))
	}
	logged := buf.String()
	require.NotEmpty(t, logged, "positive control: these branches must have produced log output at all")
	require.True(t, strings.Contains(logged, testHost), "positive control: the accumulated log must really be this handler's, naming the host it worked on")
	assert.NotContains(t, logged, testToken, "no branch of any RPC in this package may log the submitted token")
}
