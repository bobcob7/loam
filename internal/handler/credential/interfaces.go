// Package credential implements loam.admin.v1.CredentialService
// (docs/web-spec.md -> "CredentialService"): the one token per forge host
// that every repo on that host shares, covering both the REST calls that
// open upstream PRs and git-over-HTTPS transport to the upstream
// (docs/sync-spec.md -> Upstream Transport). Token-only: there is no SSH
// key here, in the store beneath it, or in the schema beneath that -- see
// the reserved field 3 in proto/loam/admin/v1/credential.proto and
// internal/credentialstore's own package doc.
//
// # Write-only by construction, not by discipline
//
// A token that reaches this package leaves it in exactly two directions:
// into internal/credentialstore's UpsertToken (which encrypts it with
// AES-GCM before any query sees it) and into the forge's ValidateToken.
// It goes nowhere else -- not into a response, not into an error, not into
// a log line.
//
// Two independent mechanisms enforce that, deliberately layered:
//
//  1. The proto cannot express a readback. All three RPCs return only
//     CredentialStatus { host, has_token, validated }; there is no field
//     any token could occupy even if this package tried.
//  2. This package's own store seam (credentialStore, below) omits
//     GetByHost -- *credentialstore.Store's ONLY decrypting method -- so
//     the plaintext of an already-stored token is not merely unused here,
//     it is unreachable. internal/handler/repoadmin and internal/mirrorsync
//     are the callers that legitimately need it; this one does not, and a
//     seam it cannot call is a stronger guarantee than a call it happens
//     not to make.
//
// Nothing anywhere in this package logs a token, and every error that
// crosses a seam carrying one is passed through redactToken before it is
// wrapped or returned (see credential.go) -- the same belt-and-braces
// applied to output, argv, the returned error AND the log line that
// internal/gittransport's scrubSecrets already establishes for the git
// subprocess path.
package credential

import (
	"context"

	"github.com/bobcob7/loam/internal/credentialstore"
)

//go:generate go tool moq -out moq_test.go . credentialStore tokenValidator

// credentialStore is the internal/credentialstore.Store surface this
// package's Handler needs, defined here at the consumer per repo
// convention. *credentialstore.Store satisfies it structurally.
//
// What is ABSENT is the load-bearing part: GetByHost, the store's only
// method that decrypts token_ciphertext back to plaintext. Adding it here
// would make a readback route expressible in this package for the first
// time; leaving it out means no method on this seam can return a token,
// so no bug, refactor, or future RPC in this package can accidentally
// route one to the wire. See the package doc.
type credentialStore interface {
	// UpsertToken encrypts token and stores it for host, inserting on
	// first use and replacing in place afterwards. It resets validated
	// to false in the same statement, so the verdict recorded for a
	// PREVIOUS token can never survive its replacement.
	UpsertToken(ctx context.Context, host, token string) (credentialstore.CredentialStatus, error)
	// GetStatus reports host's presence and validation state without
	// decrypting anything, wrapping credentialstore.ErrNotFound when the
	// host has no row at all.
	GetStatus(ctx context.Context, host string) (credentialstore.CredentialStatus, error)
	// ListStatuses reports every host's presence and validation state,
	// ordered by host, without decrypting anything.
	ListStatuses(ctx context.Context) ([]credentialstore.CredentialStatus, error)
	// SetValidated records the forge's verdict for a token already
	// written by UpsertToken. It never touches token_ciphertext.
	SetValidated(ctx context.Context, host string, validated bool) (credentialstore.CredentialStatus, error)
}

// tokenValidator confirms a candidate token authenticates against a forge
// host and carries the scope needed to open pull requests, defined here at
// the consumer. *forge.Forgejo satisfies it structurally.
//
// Note the shape: host and token are arguments, not instance state. That
// is why -- unlike internal/handler/repoadmin's upstreamChecker, which
// needs a per-call *forge.Forgejo bound to host+token because CheckRepo
// compares upstreamURL against the instance's OWN bound host -- a single,
// host-agnostic provider built once at the composition root serves every
// host here (forge.Provider's ValidateToken doc: "take their host/token
// explicitly so callers can validate ... a candidate token before it is
// bound to (or replaces) an instance's own credential").
//
// The error it returns is the ONLY channel through which this handler can
// tell an admin WHY a token was refused: CredentialStatus has no reason
// field, so forge.ErrInvalidToken and forge.ErrInsufficientScope must be
// matched with errors.Is and reported as distinct Connect codes, never
// folded together (forge/errors.go names this package as the consumer that
// does so). It is also the one seam an untrusted third party can put bytes
// into -- a forge that echoed the submitted token back in an error body
// would otherwise hand it straight to a log -- so every error out of it is
// redacted before it is wrapped; see Handler.validateToken.
type tokenValidator interface {
	ValidateToken(ctx context.Context, host, token string) error
}
