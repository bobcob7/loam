// Package urlredact holds the one implementation of "render a URL, a
// host, or a transport error without disclosing an embedded credential"
// that this repository has. It is the only thing standing between a forge
// token and the log stream, which is why it is a package rather than a
// helper copied wherever it was next needed.
//
// # Why this is shared rather than duplicated
//
// ELEVEN IMPLEMENTATIONS ACROSS SIX PACKAGES became the six functions
// below.
//
// THE COUNTING RULE, stated because a number without one gets recounted
// differently and is wrong again: this counts NON-TEST FUNCTION
// DEFINITIONS DELETED by the extraction whose body moved into this
// package. It is mechanical -- `git diff <before> <after> | grep '^-func '`
// minus Test* -- and not a judgement about which of them "really" count as
// redaction. Test functions are excluded because they MOVED rather than
// being consolidated. Under that rule:
//
//	[URL]             <- redactUserinfo    x3  (repoadmin, forge, fakeforge)
//	[URLString]       <- redactURLString       (forge)
//	                     redact                (cmd/demoenv)
//	[Host]            <- redactHost            (forge)
//	[Secrets]         <- userinfoSecrets       (forge)
//	[Scrub]           <- scrubUserinfo         (forge)
//	                     scrubSecrets          (gittransport)
//	                     redactToken           (handler/credential)
//	[TransportError]  <- redactTransportError  (forge)
//
// Eleven definitions, six functions. FIVE of the eleven were redundant
// duplicates of a sibling (two extra redactUserinfo, one extra
// URLString-shaped, two extra Scrub-shaped); the other six are one per
// resulting function. Three of those six -- Host, Secrets and
// TransportError -- had only ever been implemented once, in
// internal/forge, which alone held six of the eleven.
//
// Every one of the three multi-implementation families was confirmed
// equivalent by running the deleted body against the shared one over a
// generated corpus, rather than by reading them -- a different signature
// (scrubSecrets was variadic where scrubUserinfo took a slice) sometimes
// means different semantics, and here it did not.
//
// THE ORDER MATTERS more than the total. redactUserinfo's three copies
// were each added by a different change, none of which could see the other
// two, and the THIRD copy's own comment said its arrival was the moment to
// extract a shared one. That moment passed unnoticed. Worse, consolidating
// the first nine made the last two visible only afterwards: the bead that
// called this three copies was itself an undercount, and so was the first
// attempt at correcting it.
//
// Duplicated security-critical logic means the next hardening lands in
// some of the copies and nobody can tell which. It also HIDES COVERAGE
// HOLES, measured rather than argued: before the extraction, reintroducing
// the leak into internal/fakeforge's own copy left that package's 119
// tests entirely green while the same mutation killed named tests in
// internal/forge and internal/handler/repoadmin. One function means one
// mutation reaches every call site (loam-051m; loam-ldx is the precedent
// for when duplication of a security-relevant helper stops being
// acceptable).
//
// # The trap this package exists to carry
//
// net/url reads a scheme-less string as a PATH, not an authority. So the
// obvious guard -- parse it, check u.User -- does not work on a
// host-shaped string:
//
//	u, _ := url.Parse("token@git.example.com")
//	u.User   // nil
//	u.Host   // ""
//	u.Path   // "token@git.example.com"
//
// The credential is fully live, sitting in Path, and a check on u.User
// passes it straight through. Something like internal/forge's apiBaseURL
// then splices that whole string into "https://token@git.example.com/api/v1/..."
// and the token is in every subsequent error and log line.
//
// That is the entire reason [Host] exists and is not simply [URLString]:
// [Host] supplies a scheme before parsing. Any future userinfo check on a
// host-shaped string in this repository hits the same trap, so reach for
// [Host] rather than writing the check again.
//
// # Two independent layers
//
// STRUCTURAL redaction ([URL], [URLString], [Host], and the rebuild inside
// [TransportError]) removes the userinfo component and re-renders. It is
// exact, and it preserves error chains.
//
// SECRET SCRUBBING ([Secrets] plus [Scrub]) removes the credential's
// literal text from arbitrary strings -- git's stderr, an inner error, a
// proxy quoting the request back. It reaches what the structural layer
// cannot, at the cost of needing the secrets in hand.
//
// Neither alone is sufficient; [TransportError] applies both, in that
// order.
package urlredact

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// unparseableURLPlaceholder stands in for a URL that could not be parsed,
// and therefore could not be inspected for an embedded credential.
const unparseableURLPlaceholder = "<unparseable-url>"

// Marker replaces a credential wherever [Scrub] finds one.
//
// NO LEAK TEST MAY ASSERT ON THIS VALUE. The redaction tests assert on the
// ABSENCE of the secret, deliberately, so the marker stays free to change
// shape while a leak always fails (see internal/forge's
// userinfo_leak_test.go for why that is the design and not an omission).
//
// THAT PROPERTY IS ONLY TRUE BECAUSE EVERY POSITIVE CONTROL NAMES THIS
// CONSTANT, and it is checked rather than asserted: changing this value
// and nothing else is SURVIVED by all eight dependent packages, 520 tests
// run. It was not true when this constant was first exported --
// internal/gittransport's TestRun_ScrubsEveryFormOfTheSecret still had a
// bare "[REDACTED]" literal, five lines from a line the extraction had
// edited, and that one assertion falsified the whole claim. If you add a
// positive control, spell it Marker, or you silently re-break this.
//
// It is exported for that positive-control case, which is legitimate and
// which internal/handler/credential's and internal/gittransport's tests
// need: an assertion that a redacted message is the one that reached the
// log, rather than an empty string that would pass an absence check
// vacuously. One exported name is better than the second and third
// spellings of the literal that used to sit in those packages, one of them
// under a comment promising it was "deliberately the same one".
const Marker = "[REDACTED]"

// URL reconstructs u's string form with any embedded userinfo (user, or
// user:password) cleared, rather than string-replacing the password
// component -- which fails for the empty-password PAT form
// "https://<token>@host/path" (no ":" for a naive replace to find).
// Safe to render in an error message or a log line.
func URL(u *url.URL) string {
	redacted := *u
	redacted.User = nil
	return redacted.String()
}

// URLString parses raw and returns its redacted form (see [URL]). If raw
// fails to parse, a fixed placeholder is returned instead of raw itself:
// returning raw on the parse-failure path would be exactly the leak
// redaction exists to prevent, since a parse failure says nothing about
// whether raw embeds a credential.
func URLString(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return unparseableURLPlaceholder
	}
	return URL(u)
}

// Host returns hostOrURL in a form safe to render in an error or a log
// line. It is the host-shaped counterpart to [URLString]: a host string
// may be a bare domain, a domain:port, or a scheme-qualified origin, and
// a caller such as internal/forge's apiBaseURL turns any of those into a
// request URL.
//
// The userinfo-free case returns hostOrURL UNCHANGED rather than a
// re-rendered parse of it, so every existing message is byte-identical and
// this can be applied at every call site as pure defence in depth. A host
// that does carry userinfo collapses to its bare authority, which is all
// any of those messages needed from it.
//
// A missing scheme is supplied before parsing because net/url reads a
// scheme-less string as a PATH -- the trap this package's doc comment
// spells out with the failing example. "token@host" parses with
// User == nil and would sail through an unprefixed check.
func Host(hostOrURL string) string {
	probe := hostOrURL
	if !strings.Contains(probe, "://") {
		probe = "https://" + probe
	}
	u, err := url.Parse(probe)
	if err != nil {
		return unparseableURLPlaceholder
	}
	if u.User == nil {
		return hostOrURL
	}
	return u.Host
}

// Secrets returns every distinct string form of u's embedded credential
// that could appear in something derived from a request to u. All three
// forms are needed because different layers echo different ones:
//
//   - u.User.String() is the wire form, percent-encoded -- which is what a
//     URL rendered back out carries, and the form that defeats a naive
//     password-only redaction when a token itself contains a ":" (see
//     internal/gittransport's transport_test.go on the "user%3Atoken" case).
//   - Username() is the DECODED username -- the position a Forgejo/GitHub/
//     GitLab PAT actually occupies in the standard "https://<token>@host"
//     spelling, and the one git echoes verbatim when it prompts (see
//     internal/forge's lsRemoteProbeOverGit).
//   - Password() is the decoded password, the only position net/http's own
//     stripPassword and net/url's Redacted ever mask.
//
// The combined form is returned FIRST so [Scrub] replaces the longest
// match before its components, leaving no half-redacted remnant behind.
func Secrets(u *url.URL) []string {
	if u == nil || u.User == nil {
		return nil
	}
	secrets := []string{u.User.String()}
	if username := u.User.Username(); username != "" {
		secrets = append(secrets, username)
	}
	if password, ok := u.User.Password(); ok && password != "" {
		secrets = append(secrets, password)
	}
	return secrets
}

// Scrub returns s with every non-empty entry of secrets replaced by
// [Marker]. The empty-string guard is load-bearing rather than
// defensive tidiness: strings.ReplaceAll(s, "", marker) splices the marker
// between every rune of s, which would mangle an unrelated message beyond
// reading. This guard is why internal/handler/credential's redactToken,
// whose whole body was an early return for that case plus one
// ReplaceAll, could be absorbed here without changing what it did.
//
// It is variadic so both original call shapes reach it unchanged: a
// caller holding a []string from [Secrets] spreads it, and a caller
// naming its secrets individually (internal/gittransport's git-subprocess
// scrubbing, which passes token, password and the base64 auth header)
// lists them.
func Scrub(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, secret, Marker)
	}
	return s
}

// TransportError returns err in a form safe to log AND safe to %w-wrap.
// Wrapping matters as much as logging here: a %w chain rendered by a
// handler, or collapsed into an RPC error message, discloses a credential
// exactly as effectively as a log line does (loam-9h1e).
//
// THE DEFECT THIS EXISTS FOR. net/http returns transport failures as
// *url.Error, whose Error() renders the request URL through net/http's
// stripPassword -- which masks the PASSWORD COMPONENT ONLY. A token in the
// userinfo USERNAME position, which is the standard PAT-in-URL spelling for
// every forge this repository supports, passes through completely unmasked.
// net/url's own Redacted() has the identical blind spot, documented at
// length in internal/gittransport's validateUpstreamURL.
//
// TWO LAYERS, IN THIS ORDER, because neither alone is sufficient:
//
//  1. STRUCTURAL. When err is a top-level *url.Error whose URL parses and
//     carries userinfo, it is rebuilt with the userinfo-free rendering and
//     the SAME inner error. This is the layer that matters in practice: it
//     preserves the unwrap chain, so errors.Is against what the transport
//     actually reported (a cancelled context, http.ErrSchemeMismatch --
//     which internal/forge's Forgejo.ValidateToken plaintext-HTTP retry
//     depends on) survives redaction untouched.
//  2. SCRUBBING. Whatever the first layer produced is then swept for the
//     secrets themselves -- both those the caller knows (extra, typically
//     [Secrets] of the URL it built the request from) and those recoverable
//     from the *url.Error's own URL field. This catches a credential the
//     structural rewrite cannot reach: one echoed by the INNER error, by a
//     nested wrapper, or by a proxy/TLS layer quoting the request back.
//
// If scrubbing had to change anything, the chain is DROPPED and a plain
// errors.New is returned. That is deliberate and follows
// internal/handler/credential's redactErr precedent: an error that redacts
// its own Error() while still wrapping the original hands the plaintext to
// anyone who calls errors.Unwrap, or formats it with %+v. Losing errors.Is
// on a path that was already leaking is a strictly better trade than
// keeping it.
//
// A *url.Error whose URL field does not parse is handled the way
// [URLString] handles the same case, and for the same reason: a parse
// failure says nothing about whether the string embeds a credential, so the
// URL is dropped entirely rather than rendered.
func TransportError(err error, extra []string) error {
	if err == nil {
		return nil
	}
	// extra is COPIED rather than appended to in place: a caller's slice
	// with spare capacity would otherwise have its backing array written
	// through by the append below, which is a silent action-at-a-distance
	// bug waiting for the first caller that reuses one.
	secrets := append([]string(nil), extra...)
	var uerr *url.Error
	if errors.As(err, &uerr) {
		u, parseErr := url.Parse(uerr.URL)
		if parseErr != nil {
			return fmt.Errorf("%s %s: %s", uerr.Op, unparseableURLPlaceholder, Scrub(uerr.Err.Error(), secrets...))
		}
		secrets = append(secrets, Secrets(u)...)
		if err == error(uerr) && u.User != nil {
			err = &url.Error{Op: uerr.Op, URL: URL(u), Err: uerr.Err}
		}
	}
	rendered := err.Error()
	if scrubbed := Scrub(rendered, secrets...); scrubbed != rendered {
		return errors.New(scrubbed)
	}
	return err
}
