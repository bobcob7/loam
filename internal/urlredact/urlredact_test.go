package urlredact

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentinelToken is the value every test in this file puts in the USERINFO
// USERNAME position -- the shape net/http's own stripPassword does NOT
// mask, since it masks only the password component (loam-9h1e). It is
// deliberately long and unmistakable so a substring search for it cannot
// match by accident.
//
// EVERY assertion here is on the ABSENCE OF THIS VALUE, never on the
// presence of a particular mask. That distinction is the whole design: the
// marker redaction happens to use today is an implementation detail that
// should be free to change shape, while a leak must fail the test no
// matter which of the several redaction routes (structural URL rebuild,
// secret scrubbing, output suppression) actually caught it -- or failed
// to. Each test pairs that negative assertion with a POSITIVE CONTROL on
// something the same string must still contain (the host, the transport's
// own reason), so a redaction that silently emptied the message could not
// pass vacuously.
//
// These tests, and this rule, moved here verbatim from internal/forge's
// userinfo_leak_test.go when the helpers they cover were extracted into
// this package (loam-051m). The end-to-end leak tests that drive real
// providers stayed behind, where the providers are.
const sentinelToken = "SENTINEL-loam9h1e-4f2b8c1d9e7a" //nolint:gosec // test fixture, not a real credential

// TestSchemelessUserinfoParsesAsAPath makes the trap in this package's doc
// comment executable rather than prose: it is the whole reason [Host]
// exists as something other than a thin wrapper over [URLString], and it
// is the exact assumption a future author would otherwise have to
// discover by leaking a credential. If net/url ever changed this, the
// scheme-prefixing in Host would become dead weight and this test would
// say so.
func TestSchemelessUserinfoParsesAsAPath(t *testing.T) {
	t.Parallel()
	u, err := url.Parse(sentinelToken + "@git.example.com")
	require.NoError(t, err, "it parses -- that is what makes it dangerous")
	assert.Nil(t, u.User, "the obvious guard, u.User != nil, does NOT fire on a scheme-less string")
	assert.Empty(t, u.Host, "net/url read the whole thing as a path")
	assert.Contains(t, u.Path, sentinelToken, "the credential is live, sitting in Path")
}

// TestURLAndURLStringStripUserinfo pins the two structural entry points on
// the shape that defeats a password-only redaction: an empty-password PAT
// URL, where there is no ":" for a naive string replace to find.
func TestURLAndURLStringStripUserinfo(t *testing.T) {
	t.Parallel()
	raw := "https://" + sentinelToken + "@forge.example.invalid/acme/widgets"
	u, err := url.Parse(raw)
	require.NoError(t, err)
	for name, got := range map[string]string{"URL": URL(u), "URLString": URLString(raw)} {
		assert.NotContains(t, got, sentinelToken, "%s leaked", name)
		assert.Contains(t, got, "forge.example.invalid/acme/widgets", "positive control: %s must keep everything that was not the credential", name)
	}
}

// TestURLStringDropsAnUnparseableURLRatherThanEchoIt pins the
// parse-failure path: returning raw would be exactly the leak this
// function exists to prevent, because a parse failure says nothing about
// whether raw embeds a credential.
func TestURLStringDropsAnUnparseableURLRatherThanEchoIt(t *testing.T) {
	t.Parallel()
	raw := "https://" + sentinelToken + "@forge.example.invalid/acme/\x7f"
	_, parseErr := url.Parse(raw)
	require.Error(t, parseErr, "precondition: this URL must be one net/url rejects")
	assert.NotContains(t, URLString(raw), sentinelToken)
}

// TestUserinfoSecretsCoversBothPositions pins the helper the scrubbing
// layer depends on: it must surface the username and the password
// SEPARATELY as well as the combined encoded form, since git and net/http
// each echo a different one of the three.
func TestUserinfoSecretsCoversBothPositions(t *testing.T) {
	t.Parallel()
	u := &url.URL{Scheme: "https", User: url.UserPassword("alice", sentinelToken), Host: "forge.example.invalid"}
	got := Secrets(u)
	assert.Contains(t, got, "alice")
	assert.Contains(t, got, sentinelToken)
	assert.Contains(t, got, u.User.String())
	assert.Nil(t, Secrets(&url.URL{Scheme: "https", Host: "forge.example.invalid"}), "a URL with no userinfo has no secrets to scrub")
	assert.Nil(t, Secrets(nil))
}

// TestScrubUserinfoEmptySecretIsANoOp pins the guard that stops
// strings.ReplaceAll(s, "", marker) from splicing the marker between every
// rune of an unrelated message. internal/handler/credential's redactToken
// was an early return for exactly this case plus one ReplaceAll, which is
// why absorbing it here changed nothing. Its own no-op test was this test
// under another name, so it was removed rather than duplicated.
func TestScrubUserinfoEmptySecretIsANoOp(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "connection refused", Scrub("connection refused", ""))
	assert.Equal(t, "connection refused", Scrub("connection refused"))
	assert.Equal(t, "connection refused", Scrub("connection refused", []string(nil)...))
}

// TestScrubReplacesTheLongestFormFirst pins the ordering [Secrets]
// promises: scrubbing the combined "user:password" wire form before its
// components is what stops a half-redacted remnant -- the username left
// standing beside a masked password -- from being left behind.
func TestScrubReplacesTheLongestFormFirst(t *testing.T) {
	t.Parallel()
	u := &url.URL{Scheme: "https", User: url.UserPassword("alice", sentinelToken), Host: "forge.example.invalid"}
	got := Scrub("fatal: could not authenticate as alice:"+sentinelToken+" upstream", Secrets(u)...)
	assert.NotContains(t, got, sentinelToken)
	assert.NotContains(t, got, "alice:", "the combined form must be replaced whole, not left as a half-redacted remnant")
	assert.Contains(t, got, "upstream", "positive control: non-secret text must survive")
}

// TestRedactTransportErrorKeepsTheChainWhenItSafelyCan pins the property
// the structural rewrite exists for: redacting a *url.Error must not cost
// callers errors.Is against what the transport actually reported.
//
// THE ONE DEPENDENT, verified by reading it rather than assumed:
// internal/forge's Forgejo.ValidateToken matches
// errors.Is(err, http.ErrSchemeMismatch) on the error probeValidateToken
// returns -- now through a redacting wrap -- to decide whether to retry a
// scheme-less host over plain HTTP. If redaction flattened that error, a
// self-hosted forge with no TLS in front of it would silently stop
// validating; internal/forge keeps its own end-to-end test of exactly that
// (TestValidateTokenSchemeMismatchRetrySurvivesRedaction), because it is
// the caller. context.Canceled is used below only as a convenient stand-in
// sentinel for "the inner error is still reachable"; it is NOT itself a
// case any caller depends on.
//
// An earlier version of this comment cited receivePackProbeOverGit's
// classification instead -- specific, verified-sounding, and never checked
// against the function it named. That is the SAME defect loam-po8e's own
// test comment had, which is worth recording rather than quietly fixing:
// the shape is very hard to see from inside the change that introduces it.
// The rule that catches it is to state the scope actually checked, not the
// general claim that scope suggests.
func TestRedactTransportErrorKeepsTheChainWhenItSafelyCan(t *testing.T) {
	t.Parallel()
	err := &url.Error{Op: "Get", URL: "https://" + sentinelToken + "@forge.example.invalid/acme/widgets", Err: context.Canceled}
	got := TransportError(err, nil)
	assert.NotContains(t, got.Error(), sentinelToken)
	assert.ErrorIs(t, got, context.Canceled, "the transport's own error must remain matchable after redaction")
	assert.Contains(t, got.Error(), "forge.example.invalid", "positive control: the host must survive redaction")
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
	got := TransportError(err, []string{sentinelToken})
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
	got := TransportError(err, nil)
	assert.Equal(t, err.Error(), got.Error())
	assert.ErrorIs(t, got, context.DeadlineExceeded)
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
	_ = TransportError(&url.Error{Op: "Get", URL: "https://" + sentinelToken + "@forge.example.invalid/x", Err: errors.New("boom")}, extra)
	require.Equal(t, 4, cap(extra), "precondition: the slice must have spare capacity for an in-place append to be able to use")
	assert.Equal(t, []string{"caller-supplied-secret", "", "", ""}, extra[:cap(extra)],
		"TransportError appended through the caller's backing array")
}

// TestRedactHostLeavesAUserinfoFreeHostByteIdentical is what makes it
// honest to apply [Host] at every host-rendering site as defence in depth:
// for a host of any accepted shape that carries no credential, it must
// return the input UNCHANGED, so no operator-facing message shifts.
func TestRedactHostLeavesAUserinfoFreeHostByteIdentical(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"git.example.com", "git.example.com:3000", "https://git.example.com", "http://forge.internal:3000", "github.com", ""} {
		assert.Equal(t, host, Host(host), "a host with no embedded credential must render unchanged")
	}
}

// TestRedactHostStripsUserinfoIncludingWithoutAScheme covers the trap that
// makes a naive "does this parse with a User?" check useless for a host
// string: net/url reads a scheme-less string as a PATH, so "token@host"
// parses with User == nil -- while internal/forge's apiBaseURL would go on
// to splice exactly that string into "https://token@host/api/v1/...",
// credential live.
func TestRedactHostStripsUserinfoIncludingWithoutAScheme(t *testing.T) {
	t.Parallel()
	for _, host := range []string{
		"https://" + sentinelToken + "@git.example.com",
		"http://" + sentinelToken + "@git.example.com:3000",
		sentinelToken + "@git.example.com",
		"user:" + sentinelToken + "@git.example.com",
	} {
		got := Host(host)
		assert.NotContains(t, got, sentinelToken, "Host(%q) leaked", host)
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
	got := TransportError(&url.Error{Op: "Get", URL: raw, Err: errors.New("boom")}, nil)
	assert.NotContains(t, got.Error(), sentinelToken)
	assert.Contains(t, got.Error(), "boom", "positive control: the transport's own reason must survive")
}
