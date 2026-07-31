// Package forgehost is the single canonicalization rule for a forge-host
// string, shared by the two independently-keyed lookups this repo has for
// what is conceptually one thing (docs/persistence-spec.md's
// credentials.host and repos.forge_host):
// internal/handler/credential.SetUpstreamToken, which accepts a bare
// credential host string that may or may not carry a scheme, and
// internal/handler/repoadmin's forgeHostOf, which derives a host from an
// already-parsed, scheme-validated upstream repo URL.
//
// Before this package existed (loam-0hjq) the two derivations were
// independent: SetUpstreamToken stored req.Msg.GetHost() verbatim after
// only strings.TrimSpace, so "https://git.example.com" and
// "git.example.com" produced two different credentials.host rows for what
// is really one forge -- and internal/forge's apiBaseURL tolerates a
// scheme-qualified host well enough that the https-prefixed form VALIDATES
// over the wire and reports validated=true, so an admin who pasted a forge
// URL saw a working-looking credential that ProbeRepo/EnrollRepo (which
// always derive the bare form for an https upstream) could never find
// (credentialstore.GetByHost is an exact string match with no
// normalization). This package makes the two ends agree by construction.
//
// The rule itself, applied by both FromURL and Canonicalize: bare
// "host:port" for the default, https, scheme -- byte-for-byte what every
// https workflow in this repo has always used -- and scheme-qualified
// "http://host:port" only for plain HTTP, because internal/forge's
// apiBaseURL dials a scheme-less host over https and has no other way to
// address a plaintext forge. See forgeHostOf's own doc comment in
// internal/handler/repoadmin/handler.go, which this package's rule exists
// to keep matching byte for byte -- forgeHostOf delegates to FromURL
// rather than duplicating the decision.
package forgehost

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// errInvalid means a candidate host string could not be parsed as any of
// Canonicalize's accepted forms. Kept unexported: every caller in this
// repo wraps it with handler.ErrInvalidArgument rather than matching it
// directly (see internal/handler/credential.SetUpstreamToken), so there is
// no external consumer for errors.Is to serve yet -- unexported by default
// per repo convention.
var errInvalid = errors.New("forgehost: invalid host")

// expectedForm is repeated in every Canonicalize rejection, so an admin
// sees the same guidance regardless of which rule tripped.
const expectedForm = "expected a bare host[:port], or an http(s) URL with no path, query, or userinfo"

// FromURL returns the canonical forge-host string for u, an already
// scheme-validated URL (u.Scheme is "http" or "https"; both of this
// package's callers -- internal/handler/repoadmin's forgeHostOf and
// deriveRepoIdentity -- check that before calling this). This is the
// bare-vs-scheme-qualified half of the package doc's rule, applied to an
// upstream repo URL that is already fully parsed; u.Path (the
// "<group>/<repo_name>" part of a full repo URL) is deliberately ignored,
// exactly as forgeHostOf always has.
func FromURL(u *url.URL) string {
	return canonical(u.Scheme, u.Host)
}

// canonical is the one rule both FromURL and Canonicalize apply: bare host
// for everything except plain http, which keeps its scheme prefix (see the
// package doc for why).
func canonical(scheme, host string) string {
	if scheme == "http" {
		return "http://" + host
	}
	return host
}

// Canonicalize parses raw -- a bare credential host string as typed into
// the Credentials screen's Host field or sent as
// SetUpstreamTokenRequest.host, which may or may not carry a scheme --
// into the same canonical form FromURL derives from a full upstream URL,
// so credentials.host and repos.forge_host agree by construction rather
// than by operator discipline. See the package doc for the underlying
// rule and why it exists.
//
// Accepted:
//   - a bare host, optionally with a port ("git.example.com",
//     "git.example.com:3000") -- canonicalizes to itself.
//   - an https URL with no path/query/fragment/userinfo
//     ("https://git.example.com") -- canonicalizes to the bare host.
//   - an http URL with no path/query/fragment/userinfo
//     ("http://git.example.com:3000") -- canonicalizes to itself (the
//     scheme-qualified form), since apiBaseURL only ever dials a
//     scheme-less host over https.
//
// A bare trailing "/" with nothing after it is tolerated (the common
// address-bar-copy case, "https://git.example.com/"); anything else in
// the path, or a query or fragment, is rejected as a differently-SHAPED
// input, not a differently spelled one.
//
// Rejected, with an error naming the expected form and never echoing raw
// (raw may carry a credential in its userinfo component -- loam-ra1k):
//   - a scheme other than http/https.
//   - a path (beyond a bare trailing "/"), query, or fragment component.
//   - embedded userinfo ("https://token@host").
//   - anything unparseable, or an empty host once parsed.
func Canonicalize(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("forgehost: host is empty: %w", errInvalid)
	}
	scheme, rest := "https", trimmed
	if strings.Contains(trimmed, "://") {
		parts := strings.SplitN(trimmed, "://", 2)
		scheme, rest = parts[0], parts[1]
		if scheme != "http" && scheme != "https" {
			return "", fmt.Errorf("forgehost: scheme %q must be http or https: %w", scheme, errInvalid)
		}
	}
	u, err := url.Parse(scheme + "://" + rest)
	if err != nil {
		return "", fmt.Errorf("forgehost: unparseable host: %s: %w", expectedForm, errInvalid)
	}
	if u.User != nil {
		return "", fmt.Errorf("forgehost: host must not carry embedded userinfo: %s: %w", expectedForm, errInvalid)
	}
	if u.Host == "" {
		return "", fmt.Errorf("forgehost: host is empty: %w", errInvalid)
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("forgehost: host must not carry a path, query, or fragment: %s: %w", expectedForm, errInvalid)
	}
	return canonical(scheme, u.Host), nil
}
