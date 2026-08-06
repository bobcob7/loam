package forge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentinelToken is the value every test in this file puts in the USERINFO
// USERNAME position of an upstream URL -- the shape net/http's own
// stripPassword does NOT mask, since it masks only the password component
// (loam-9h1e). It is deliberately long and unmistakable so a substring
// search for it cannot match by accident.
//
// EVERY assertion in this file is on the ABSENCE OF THIS VALUE, never on
// the presence of a particular mask. That distinction is the whole design:
// the marker redaction happens to use today is an implementation detail
// that should be free to change shape, while a leak must fail the test no
// matter which of the several redaction routes (structural URL rebuild,
// secret scrubbing, output suppression) actually caught it -- or failed to.
// Each test pairs that negative assertion with a POSITIVE CONTROL on
// something the same string must still contain (the host, a known log
// message), so a redaction that silently emptied the message could not
// pass vacuously.
const sentinelToken = "SENTINEL-loam9h1e-4f2b8c1d9e7a" //nolint:gosec // test fixture, not a real credential

// userinfoShapes is the set of userinfo forms an upstream URL can carry.
// The username-only form is the one this bead is about: it is the standard
// PAT-in-URL spelling for Forgejo, GitHub and GitLab, and the one every
// password-masking redaction (net/url's Redacted, net/http's stripPassword)
// passes through verbatim. The user:password form is included so a fix that
// only handled the username position would still be caught.
var userinfoShapes = []struct {
	name   string
	userOf func(secret string) *url.Userinfo
}{
	{name: "username only (the PAT-in-URL form)", userOf: func(secret string) *url.Userinfo { return url.User(secret) }},
	{name: "username and password", userOf: func(secret string) *url.Userinfo { return url.UserPassword("attacker-user", secret) }},
	{name: "percent-encoded colon in the username", userOf: func(secret string) *url.Userinfo { return url.User("user:" + secret) }},
}

// recordingBuffer is a mutex-guarded sink for slog output. slog writes on
// the calling goroutine, but the buffer is read after the call under -race
// and the guard makes that unambiguous rather than merely true today.
type recordingBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recordingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *recordingBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// recordingLogger returns a DEBUG-level logger and the buffer it writes
// to. Debug level is load-bearing: every probe log line this bead is about
// is emitted with DebugContext, so a default-level handler would drop all
// of them and the "no log record contains the sentinel" assertion would
// pass without ever having seen a record.
func recordingLogger() (*slog.Logger, *recordingBuffer) {
	buf := &recordingBuffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// assertNoSentinel asserts that the sentinel appears nowhere err can be
// rendered from: its own message, every error in its unwrap chain (the
// "one Unwrap away" hole -- an error that redacts its own Error() while
// still wrapping the original hands the plaintext to anyone who calls
// errors.Unwrap), and its %+v form.
func assertNoSentinel(t *testing.T, err error, logs string) {
	t.Helper()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), sentinelToken, "the sentinel reached the returned error string")
	assert.NotContains(t, fmt.Sprintf("%+v", err), sentinelToken, "the sentinel reached the error's %%+v rendering")
	for e := err; e != nil; e = errors.Unwrap(e) {
		assert.NotContains(t, e.Error(), sentinelToken, "the sentinel is reachable from the error chain via errors.Unwrap")
	}
	assert.NotContains(t, logs, sentinelToken, "the sentinel reached a log record")
}

// TestReceivePackProbe_TransportError_NeverEmitsUserinfoSentinel is the
// direct regression test for loam-9h1e's named site. A TRANSPORT failure
// (nothing listening) is what makes http.Client.Do return a *url.Error,
// whose Error() renders the full request URL through stripPassword -- which
// masks the password component only, leaving a username-position token in
// the clear. Every other receive-pack branch returns a status-derived error
// that never renders the URL, which is why the pre-existing redaction tests
// (which drive a 403) did not catch this.
func TestReceivePackProbe_TransportError_NeverEmitsUserinfoSentinel(t *testing.T) {
	t.Parallel()
	for _, shape := range userinfoShapes {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			unreachable := server.URL
			server.Close() // nothing is listening here anymore
			u, err := url.Parse(unreachable)
			require.NoError(t, err)
			u.User = shape.userOf(sentinelToken)
			logger, logs := recordingLogger()
			f := NewForgejo(unreachable, "bound-token", http.DefaultClient, logger)
			err = f.receivePackProbe(t.Context(), u)
			require.Error(t, err)
			assert.Contains(t, err.Error(), u.Host, "positive control: the error must still name the host it failed against")
			assert.Contains(t, logs.String(), "receive-pack probe transport error", "positive control: the transport-error log line must actually have been emitted")
			assertNoSentinel(t, err, logs.String())
		})
	}
}

// TestLsRemoteProbe_AuthChallenge_NeverEmitsUserinfoSentinel is the second
// leak this bead's sweep found, by the same route but a different carrier.
// git strips userinfo from most of its own messages ("unable to access",
// "repository ... not found", "Authentication failed") -- which is what the
// pre-existing loam-po8e test observed and generalized from. It does NOT
// strip it when it needs to PROMPT: against a 401 challenge with a
// username but no password in the URL, git emits
//
//	fatal: could not read Password for 'https://<token>@host': terminal prompts disabled
//
// verbatim, because the username is exactly what it is telling the operator
// it already has. lsRemoteProbeOverGit folds git's combined output into
// both a debug log field and the returned error string, so that message
// carried the token into both.
//
// A 401 is not an exotic branch: it is what a wrong, expired or revoked
// bound token produces, i.e. the single most likely way this probe fails in
// production.
func TestLsRemoteProbe_AuthChallenge_NeverEmitsUserinfoSentinel(t *testing.T) {
	t.Parallel()
	for _, shape := range userinfoShapes {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			requireGit(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("WWW-Authenticate", `Basic realm="forge-test"`)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			t.Cleanup(server.Close)
			u, err := url.Parse(server.URL)
			require.NoError(t, err)
			u.User = shape.userOf(sentinelToken)
			logger, logs := recordingLogger()
			f := NewForgejo(server.URL, "", server.Client(), logger)
			err = f.lsRemoteProbe(t.Context(), u.String())
			require.Error(t, err)
			assert.Contains(t, err.Error(), u.Host, "positive control: the error must still name the host it failed against")
			assert.Contains(t, logs.String(), "ls-remote probe failed", "positive control: the failure log line must actually have been emitted")
			assertNoSentinel(t, err, logs.String())
		})
	}
}

// TestCheckRepo_AuthChallenge_NeverEmitsUserinfoSentinel drives the same
// 401 through the EXPORTED entry point both providers share, rather than
// the unexported probe, so the guarantee is pinned at the API boundary a
// caller actually reaches -- CheckRepo is the only method in this package
// that takes a full repository URL, and therefore the only one an operator
// can hand a userinfo-bearing value to.
func TestCheckRepo_AuthChallenge_NeverEmitsUserinfoSentinel(t *testing.T) {
	t.Parallel()
	requireGit(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="forge-test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	u.User = url.User(sentinelToken)
	u.Path = "/acme/widgets"
	logger, logs := recordingLogger()
	f := NewForgejo(server.URL, "bound-token", server.Client(), logger)
	err = f.CheckRepo(t.Context(), u.String())
	require.Error(t, err)
	assert.Contains(t, err.Error(), u.Host, "positive control: the error must still name the host it failed against")
	// The LOG control is as load-bearing here as it is in the two probe
	// tests above, and this test shipped without it for one round: every
	// line at issue is emitted with DebugContext, so a handler that dropped
	// debug records would leave assertNoSentinel searching an empty string
	// and reporting success. Confirmed by dropping recordingLogger to the
	// default level -- with this line, this test fails alongside its
	// siblings; without it, it passed alone.
	assert.Contains(t, logs.String(), "ls-remote probe failed", "positive control: the failure log line must actually have been emitted")
	assertNoSentinel(t, err, logs.String())
}

// TestValidateToken_TransportError_NeverEmitsUserinfoSentinel covers the
// REST half of the package. internal/forgehost.Canonicalize rejects a host
// carrying userinfo at credential write time, and repoadmin derives every
// other host through forgehost.FromURL (which reads u.Host and so cannot
// carry any), so this is not reachable through any caller in this tree
// today. It is pinned anyway because internal/forge is a library package
// whose ValidateToken takes host as an explicit parameter: the *url.Error
// route into an error string is identical to the one that WAS reachable,
// and the only thing standing between them is a validation that lives in a
// different package.
func TestValidateToken_TransportError_NeverEmitsUserinfoSentinel(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable := server.URL
	server.Close()
	u, err := url.Parse(unreachable)
	require.NoError(t, err)
	host := u.Scheme + "://" + sentinelToken + "@" + u.Host
	logger, logs := recordingLogger()
	f := NewForgejo("", "", http.DefaultClient, logger)
	err = f.ValidateToken(t.Context(), host, "bound-token")
	require.Error(t, err)
	assert.Contains(t, err.Error(), u.Host, "positive control: the error must still name the host it failed against")
	assertNoSentinel(t, err, logs.String())
}

// TestListOpenPRs_TransportError_NeverEmitsUserinfoSentinel and
// TestDoPullRequest_TransportError_NeverEmitsUserinfoSentinel cover the two
// remaining *url.Error-into-an-error-string sites on the Forgejo REST path,
// reached through FindOpenPR and CreatePR respectively. Same reachability
// caveat as ValidateToken above: the bound host cannot carry userinfo
// today, and these exist so that stays true by this package's own doing.
func TestListOpenPRs_TransportError_NeverEmitsUserinfoSentinel(t *testing.T) {
	t.Parallel()
	f, logs := forgejoBoundToUnreachableHostWithUserinfo(t)
	_, _, _, err := f.FindOpenPR(t.Context(), "acme/widgets", "head", "main")
	assertNoSentinel(t, err, logs.String())
}

func TestDoPullRequest_TransportError_NeverEmitsUserinfoSentinel(t *testing.T) {
	t.Parallel()
	f, logs := forgejoBoundToUnreachableHostWithUserinfo(t)
	_, _, err := f.CreatePR(t.Context(), "acme/widgets", "head", "main", "t", "d")
	assertNoSentinel(t, err, logs.String())
}

// TestGitHubDoPullRequest_TransportError_NeverEmitsUserinfoSentinel is the
// GitHub-side twin of the two above: github.go has its own doPullRequest
// with the same `calling %s %s: %w` shape, so a fix applied only to
// forgejo.go would leave this one behind.
func TestGitHubDoPullRequest_TransportError_NeverEmitsUserinfoSentinel(t *testing.T) {
	t.Parallel()
	host, logger, logs := unreachableHostWithUserinfo(t)
	g := NewGitHub(host, "bound-token", http.DefaultClient, logger)
	_, _, err := g.CreatePR(t.Context(), "acme/widgets", "head", "main", "t", "d")
	assertNoSentinel(t, err, logs.String())
}

// TestGitHubValidateToken_TransportError_NeverEmitsUserinfoSentinel and
// TestGitHubFindOpenPR_TransportError_NeverEmitsUserinfoSentinel close the
// last two http.Client.Do sites in the package.
func TestGitHubValidateToken_TransportError_NeverEmitsUserinfoSentinel(t *testing.T) {
	t.Parallel()
	host, logger, logs := unreachableHostWithUserinfo(t)
	g := NewGitHub("", "", http.DefaultClient, logger)
	err := g.ValidateToken(t.Context(), host, "bound-token")
	assertNoSentinel(t, err, logs.String())
}

func TestGitHubFindOpenPR_TransportError_NeverEmitsUserinfoSentinel(t *testing.T) {
	t.Parallel()
	host, logger, logs := unreachableHostWithUserinfo(t)
	g := NewGitHub(host, "bound-token", http.DefaultClient, logger)
	_, _, _, err := g.FindOpenPR(t.Context(), "acme/widgets", "head", "main")
	assertNoSentinel(t, err, logs.String())
}

// unreachableHostWithUserinfo returns a scheme-qualified host string with
// the sentinel in its userinfo username position, pointing at an address
// nothing is listening on, plus a recording logger. A closed httptest
// server is used rather than a made-up port so the address is guaranteed
// unused rather than merely likely to be.
func unreachableHostWithUserinfo(t *testing.T) (string, *slog.Logger, *recordingBuffer) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	server.Close()
	logger, logs := recordingLogger()
	return u.Scheme + "://" + sentinelToken + "@" + u.Host, logger, logs
}

// forgejoBoundToUnreachableHostWithUserinfo builds a *Forgejo bound to such
// a host, for the REST methods that read the bound host rather than taking
// one as a parameter.
func forgejoBoundToUnreachableHostWithUserinfo(t *testing.T) (*Forgejo, *recordingBuffer) {
	t.Helper()
	host, logger, logs := unreachableHostWithUserinfo(t)
	return NewForgejo(host, "bound-token", http.DefaultClient, logger), logs
}

// TestUserinfoSecretsCoversBothPositions pins the helper the scrubbing
// layer depends on: it must surface the username and the password
// SEPARATELY as well as the combined encoded form, since git and net/http
// each echo a different one of the three.
func TestUserinfoSecretsCoversBothPositions(t *testing.T) {
	t.Parallel()
	u := &url.URL{Scheme: "https", User: url.UserPassword("alice", sentinelToken), Host: "forge.example.invalid"}
	got := userinfoSecrets(u)
	assert.Contains(t, got, "alice")
	assert.Contains(t, got, sentinelToken)
	assert.Contains(t, got, u.User.String())
	assert.Nil(t, userinfoSecrets(&url.URL{Scheme: "https", Host: "forge.example.invalid"}), "a URL with no userinfo has no secrets to scrub")
	assert.Nil(t, userinfoSecrets(nil))
}

// TestScrubUserinfoEmptySecretIsANoOp pins the guard that stops
// strings.ReplaceAll(s, "", marker) from splicing the marker between every
// rune of an unrelated message -- the same trap internal/handler/credential
// documents for its own redactToken.
func TestScrubUserinfoEmptySecretIsANoOp(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "connection refused", scrubUserinfo("connection refused", []string{""}))
	assert.Equal(t, "connection refused", scrubUserinfo("connection refused", nil))
}

// TestRedactTransportErrorKeepsTheChainWhenItSafelyCan pins the property
// the structural rewrite exists for: redacting a *url.Error must not cost
// callers errors.Is against what the transport actually reported.
//
// THE ONE DEPENDENT IN THIS PACKAGE, verified by reading it rather than
// assumed: Forgejo.ValidateToken matches errors.Is(err, http.ErrSchemeMismatch)
// on the error probeValidateToken returns -- now through a redacting wrap --
// to decide whether to retry a scheme-less host over plain HTTP. If
// redaction flattened that error, a self-hosted forge with no TLS in front
// of it would silently stop validating. Nothing else here matches on a
// transport error: receivePackProbeOverGit classifies purely by HTTP status,
// and no caller of it matches context.Canceled. context.Canceled is used
// below only as a convenient stand-in sentinel for "the inner error is still
// reachable"; it is NOT itself a case any caller depends on.
//
// An earlier version of this comment cited receivePackProbeOverGit's
// classification instead -- specific, verified-sounding, and never checked
// against the function it named. That is the SAME defect this branch exists
// to correct in loam-po8e's own test comment, written three files away from
// the correction, which is worth recording rather than quietly fixing: the
// shape is very hard to see from inside the change that introduces it. The
// rule that catches it is to state the scope actually checked, not the
// general claim that scope suggests.
func TestRedactTransportErrorKeepsTheChainWhenItSafelyCan(t *testing.T) {
	t.Parallel()
	err := &url.Error{Op: "Get", URL: "https://" + sentinelToken + "@forge.example.invalid/acme/widgets", Err: context.Canceled}
	got := redactTransportError(err, nil)
	assert.NotContains(t, got.Error(), sentinelToken)
	assert.ErrorIs(t, got, context.Canceled, "the transport's own error must remain matchable after redaction")
	assert.Contains(t, got.Error(), "forge.example.invalid", "positive control: the host must survive redaction")
}

// TestValidateTokenSchemeMismatchRetrySurvivesRedaction is the dependent
// named above, driven end to end rather than asserted about: a scheme-less
// host naming a PLAINTEXT-HTTP forge must still validate, which only works
// if http.ErrSchemeMismatch survives probeValidateToken's redacting wrap.
// This is what would actually break if the structural rewrite were dropped
// in favour of flattening every redacted error to a string.
func TestValidateTokenSchemeMismatchRetrySurvivesRedaction(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	logger, _ := recordingLogger()
	f := NewForgejo("", "", server.Client(), logger)
	// A BARE host (no scheme) for a server that speaks only plain HTTP:
	// apiBaseURL defaults it to https, the TLS layer reports
	// http.ErrSchemeMismatch, and ValidateToken retries over http:// only
	// because that sentinel is still matchable through the wrap.
	assert.NoError(t, f.ValidateToken(t.Context(), u.Host, "some-token"))
}

// TestRedactTransportErrorDropsTheChainRatherThanLeak pins the fallback:
// when the sentinel survives the structural rewrite (because it is in the
// INNER error rather than the URL field, where no rebuild can reach it),
// redaction must drop the chain rather than hand the plaintext to
// errors.Unwrap. Losing errors.Is is the correct trade -- a chain is a
// convenience, a leaked credential is not.
func TestRedactTransportErrorDropsTheChainRatherThanLeak(t *testing.T) {
	t.Parallel()
	err := &url.Error{Op: "Get", URL: "https://forge.example.invalid/acme/widgets", Err: fmt.Errorf("proxy rejected %s", sentinelToken)}
	got := redactTransportError(err, []string{sentinelToken})
	assert.NotContains(t, got.Error(), sentinelToken)
	assert.Nil(t, errors.Unwrap(got), "the plaintext must not be one Unwrap away")
}

// TestRedactTransportErrorLeavesACleanErrorAlone pins that redaction is a
// no-op on the overwhelmingly common case -- a URL with no userinfo at all
// -- so every call site added for defence in depth changes nothing an
// operator reads, and errors.Is keeps working untouched.
func TestRedactTransportErrorLeavesACleanErrorAlone(t *testing.T) {
	t.Parallel()
	err := &url.Error{Op: "Get", URL: "https://forge.example.invalid/acme/widgets", Err: context.DeadlineExceeded}
	got := redactTransportError(err, nil)
	assert.Equal(t, err.Error(), got.Error())
	assert.ErrorIs(t, got, context.DeadlineExceeded)
}

// The four tests below cover the hardening that a mutation sweep found
// untested. They are all on paths NOT reachable with userinfo today --
// which is exactly why they are worth pinning rather than skipping: the
// only thing between them and a live leak is validation in
// internal/forgehost, a package internal/forge cannot see and does not
// import. Hardening nobody tests is one refactor away from hardening nobody
// has.

// TestKindForHost_NeverEchoesUserinfo covers resolve.go's loudest branch:
// a host that CONTAINS "github" but is neither exact alias fails naming the
// host, and that message is %q-formatted straight into an error a handler
// renders. It is the one error in this package that quotes a host on a
// branch no status code guards.
func TestKindForHost_NeverEchoesUserinfo(t *testing.T) {
	t.Parallel()
	host := "https://" + sentinelToken + "@github-mirror.internal.corp"
	_, err := KindForHost(host)
	require.Error(t, err)
	assert.ErrorIs(t, err, errUnsupportedForgeKind)
	assert.Contains(t, err.Error(), "github-mirror.internal.corp", "positive control: the host must still be named, that is the whole point of this branch")
	assert.NotContains(t, err.Error(), sentinelToken)
}

// TestNewProvider_NeverEchoesUserinfo drives the same host through the
// constructor every caller with a real host in hand actually uses, so the
// guarantee is pinned where it is consumed and not only where it is
// implemented.
//
// NewProvider's own `default:` arm is deliberately NOT covered: KindForHost
// returns KindForgejo, KindGitHub, or an error and nothing else, so that arm
// is unreachable without editing resolve.go. It is a compile-time
// exhaustiveness guard, not a branch a test can reach.
func TestNewProvider_NeverEchoesUserinfo(t *testing.T) {
	t.Parallel()
	host := "https://" + sentinelToken + "@github-mirror.internal.corp"
	provider, err := NewProvider(host, "some-token", http.DefaultClient, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	require.Error(t, err)
	assert.Nil(t, provider)
	assert.Contains(t, err.Error(), "github-mirror.internal.corp", "positive control")
	assert.NotContains(t, err.Error(), sentinelToken)
}

// TestLsRemoteProbe_UnparseableURL_NeverEmitsUserinfoSentinel covers the
// guard this change added at the top of lsRemoteProbeOverGit. It fails
// closed rather than handing the string to git for a reason specific to
// this bead: scrubbing git's own output depends on knowing which substrings
// ARE the credential, and a URL that did not parse is one whose embedded
// credential cannot be identified -- so git's output would then be unsafe to
// echo at all. Neither the raw string nor the *url.Error explaining why it
// failed may be rendered, since the latter quotes the former whole.
func TestLsRemoteProbe_UnparseableURL_NeverEmitsUserinfoSentinel(t *testing.T) {
	t.Parallel()
	raw := "https://" + sentinelToken + "@forge.example.invalid/acme/\x7f"
	_, parseErr := url.Parse(raw)
	require.Error(t, parseErr, "precondition: this URL must be one net/url rejects")
	require.Contains(t, parseErr.Error(), sentinelToken, "precondition: it is the parse error itself that would leak, which is why it is never wrapped")
	logger, logs := recordingLogger()
	f := NewForgejo("forge.example.invalid", "bound-token", http.DefaultClient, logger)
	err := f.lsRemoteProbe(t.Context(), raw)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRepoNotFound, "a URL that did not parse says nothing about whether the repo exists")
	assertNoSentinel(t, err, logs.String())
}

// TestRedactTransportErrorDoesNotWriteThroughTheCallersSlice pins the
// copy: appending in place would write the *url.Error's own userinfo into a
// caller's spare capacity, which is silent action at a distance rather than
// a visible bug. Asserted against the FULL backing array, not the caller's
// view of it, because that is the only place the damage would be visible.
func TestRedactTransportErrorDoesNotWriteThroughTheCallersSlice(t *testing.T) {
	t.Parallel()
	extra := make([]string, 1, 4)
	extra[0] = "caller-supplied-secret"
	_ = redactTransportError(&url.Error{Op: "Get", URL: "https://" + sentinelToken + "@forge.example.invalid/x", Err: errors.New("boom")}, extra)
	require.Equal(t, 4, cap(extra), "precondition: the slice must have spare capacity for an in-place append to be able to use")
	assert.Equal(t, []string{"caller-supplied-secret", "", "", ""}, extra[:cap(extra)],
		"redactTransportError appended through the caller's backing array")
}

// TestRedactHostLeavesAUserinfoFreeHostByteIdentical is what makes it
// honest to apply redactHost at every host-rendering site as defence in
// depth: for a host of any accepted shape that carries no credential, it
// must return the input UNCHANGED, so no operator-facing message shifts.
func TestRedactHostLeavesAUserinfoFreeHostByteIdentical(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"git.example.com", "git.example.com:3000", "https://git.example.com", "http://forge.internal:3000", "github.com", ""} {
		assert.Equal(t, host, redactHost(host), "a host with no embedded credential must render unchanged")
	}
}

// TestRedactHostStripsUserinfoIncludingWithoutAScheme covers the trap that
// makes a naive "does this parse with a User?" check useless for a host
// string: net/url reads a scheme-less string as a PATH, so "token@host"
// parses with User == nil -- while apiBaseURL would go on to splice exactly
// that string into "https://token@host/api/v1/...", credential live.
func TestRedactHostStripsUserinfoIncludingWithoutAScheme(t *testing.T) {
	t.Parallel()
	for _, host := range []string{
		"https://" + sentinelToken + "@git.example.com",
		"http://" + sentinelToken + "@git.example.com:3000",
		sentinelToken + "@git.example.com",
		"user:" + sentinelToken + "@git.example.com",
	} {
		got := redactHost(host)
		assert.NotContains(t, got, sentinelToken, "redactHost(%q) leaked", host)
		assert.Contains(t, got, "git.example.com", "positive control: the host itself must survive")
	}
}

// TestRedactTransportErrorHandlesAnUnparseableURL pins the branch net/url
// itself produces: a *url.Error whose URL field never parsed, so no
// userinfo can be identified in it for redaction. Returning it as-is would
// be exactly the leak this function exists to prevent -- a parse failure
// says nothing about whether the string embeds a credential -- so the URL
// is dropped entirely.
func TestRedactTransportErrorHandlesAnUnparseableURL(t *testing.T) {
	t.Parallel()
	raw := "https://" + sentinelToken + "@forge.example.invalid/\x7f"
	_, parseErr := url.Parse(raw)
	require.Error(t, parseErr, "precondition: this URL must be one net/url rejects")
	got := redactTransportError(&url.Error{Op: "Get", URL: raw, Err: errors.New("boom")}, nil)
	assert.NotContains(t, got.Error(), sentinelToken)
	assert.Contains(t, got.Error(), "boom", "positive control: the transport's own reason must survive")
}
