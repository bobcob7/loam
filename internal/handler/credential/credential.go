package credential

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/forge"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
)

// redactedMarker replaces every occurrence of a submitted token in any
// string this package is about to return or log. The literal value is
// deliberately the same one internal/gittransport's scrubSecrets uses, so
// an operator grepping logs for a leak sees one marker, not two.
const redactedMarker = "[REDACTED]"

// Handler implements adminv1connect.CredentialServiceHandler.
type Handler struct {
	credentials credentialStore
	validator   tokenValidator
	errors      *handler.ErrorMapper
	logger      *slog.Logger
}

// compile-time assertion that Handler satisfies the generated interface.
var _ adminv1connect.CredentialServiceHandler = (*Handler)(nil)

// New builds a Handler over the given seams. validator is the forge
// provider SetUpstreamToken checks a candidate token against; in
// production a single host-agnostic *forge.Forgejo, since ValidateToken
// takes its host and token explicitly (see tokenValidator).
func New(credentials credentialStore, validator tokenValidator, errors *handler.ErrorMapper, logger *slog.Logger) *Handler {
	return &Handler{credentials: credentials, validator: validator, errors: errors, logger: logger}
}

// SetUpstreamToken sets or replaces the forge token for a host, after the
// forge itself confirms the token authenticates and carries the scope
// needed to open pull requests (docs/web-spec.md -> CredentialService:
// "set/replace the forge token; the server validates the REST side
// immediately (git access is proven per repo at enrollment)").
//
// # Why validation runs BEFORE the write
//
// The obvious alternative -- store first, then validate, then record the
// verdict -- was rejected for three reasons, in increasing order of
// weight:
//
//  1. A typo would destroy a working credential. UpsertToken replaces in
//     place; there is no undo and no way to recover the old token, which
//     the admin may not have kept. Every repo on that host loses sync and
//     PR access until a correct token is pasted again.
//  2. A token the forge has already told us is worthless is pure
//     liability at rest: encrypting and persisting it buys nothing and
//     adds one more secret to leak.
//  3. Decisively: CredentialStatus has no reason field, so the ONLY
//     channel for telling an admin their token was refused -- and, per
//     forge/errors.go, for telling "does not authenticate" apart from
//     "authenticates but is underscoped" -- is a non-nil error. Once
//     rejection is an error return, returning that error AFTER having
//     persisted the rejected token is strictly worse than not having
//     written it.
//
// # Why the two writes are ordered as they are
//
// UpsertToken and SetValidated are separate statements and this handler
// runs them in that order, never the reverse and never SetValidated alone.
// UpsertToken resets validated to false in the same INSERT ... ON CONFLICT
// DO UPDATE that writes the ciphertext, so the window between the two
// writes -- including one left behind by a crash or a SetValidated failure
// -- reports the new token as present but unvalidated. That understates
// reality and is the correct direction to be wrong in: the alternative
// ordering would let a replacement token silently INHERIT the previous
// token's validated=true verdict, which is a credential the operator
// believes has been checked and has not been.
//
// # What "host" means, and the coupling to RepoAdminService.EnrollRepo
//
// host is stored VERBATIM as credentials.host, the exact string
// req.Msg.GetHost() carries after trimming whitespace -- this handler
// never rewrites, defaults, or normalizes it. That matters because
// EnrollRepo resolves this same row by an independently-derived host
// (internal/handler/repoadmin/handler.go's forgeHostOf): bare
// ("host:port") for an https upstream, scheme-qualified
// ("http://host:port") for a plain-HTTP one. For a credential meant to
// back an enrollment, host here must match that derivation exactly, or
// EnrollRepo's own GetByHost call will not find it -- there is no
// normalization chokepoint reconciling a mismatched pair (loam-4kz).
// Getting this right for an https forge needs nothing special (the bare
// form has always worked); for a plaintext-HTTP forge, host must be the
// scheme-qualified form.
//
// Separately, and only for THIS request's own token-validation call:
// internal/forge/forgejo.go's ValidateToken tolerates a bare host that
// turns out to name a plaintext-HTTP forge (a scheme-mismatch retry, on
// the same signal Go's client itself produces), so a bare host still
// validates here even against a plaintext forge. That tolerance is
// scoped to reaching the forge for validation -- it does not change what
// key the token is stored under, so it is not a second way to satisfy
// EnrollRepo's lookup.
func (h *Handler) SetUpstreamToken(ctx context.Context, req *connect.Request[adminv1.SetUpstreamTokenRequest]) (*connect.Response[adminv1.SetUpstreamTokenResponse], error) {
	if err := requireAdmin(ctx, "setting an upstream token"); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	host, token := strings.TrimSpace(req.Msg.GetHost()), req.Msg.GetToken()
	if host == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("set upstream token: host is required: %w", handler.ErrInvalidArgument))
	}
	if token == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("set upstream token for host %s: token is required: %w", host, handler.ErrInvalidArgument))
	}
	if err := h.validateToken(ctx, host, token); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	if _, err := h.credentials.UpsertToken(ctx, host, token); err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("set upstream token for host %s: %w", host, redactErr(err, token)))
	}
	status, err := h.credentials.SetValidated(ctx, host, true)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("recording the validation verdict for host %s (the token itself was stored, and reports as unvalidated until this succeeds): %w", host, redactErr(err, token)))
	}
	h.logger.InfoContext(ctx, "admin set upstream token", "host", host, "validated", status.Validated)
	return connect.NewResponse(&adminv1.SetUpstreamTokenResponse{Status: statusToProto(status)}), nil
}

// GetCredentialStatus reports one host's presence and validation state
// (docs/web-spec.md -> CredentialService). It never decrypts anything: the
// store seam this package holds has no method that could.
//
// A host with no credentials row is NOT an error. It answers
// { host, has_token: false, validated: false } -- the literal reading of
// features/credentials.feature's "Credentials are scoped per host" ("When
// I view the credential status for forgejo.example.com / Then it shows no
// credential is present"), and the only answer that lets an admin ask "is
// this host configured?" without having to interpret a 404. That is
// exactly why credentialstore.ErrNotFound is exported and matched with
// errors.Is here: absence must be told apart from a database that is down,
// which stays an error and must never be flattened into "no credential".
func (h *Handler) GetCredentialStatus(ctx context.Context, req *connect.Request[adminv1.GetCredentialStatusRequest]) (*connect.Response[adminv1.GetCredentialStatusResponse], error) {
	if err := requireAdmin(ctx, "reading a credential status"); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	host := strings.TrimSpace(req.Msg.GetHost())
	if host == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("get credential status: host is required: %w", handler.ErrInvalidArgument))
	}
	status, err := h.credentials.GetStatus(ctx, host)
	if err != nil {
		if errors.Is(err, credentialstore.ErrNotFound) {
			return connect.NewResponse(&adminv1.GetCredentialStatusResponse{
				Status: &adminv1.CredentialStatus{Host: host},
			}), nil
		}
		return nil, h.errors.ToConnectErr(fmt.Errorf("getting credential status for host %s: %w", host, err))
	}
	return connect.NewResponse(&adminv1.GetCredentialStatusResponse{Status: statusToProto(status)}), nil
}

// ListCredentials reports presence and validation state across every host
// (docs/web-spec.md -> CredentialService), ordered by host by the store's
// own query. It never decrypts anything, so the response carries no token
// material for any host regardless of how many are configured.
func (h *Handler) ListCredentials(ctx context.Context, _ *connect.Request[adminv1.ListCredentialsRequest]) (*connect.Response[adminv1.ListCredentialsResponse], error) {
	if err := requireAdmin(ctx, "listing credentials"); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	statuses, err := h.credentials.ListStatuses(ctx)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("listing credential statuses: %w", err))
	}
	out := make([]*adminv1.CredentialStatus, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, statusToProto(status))
	}
	return connect.NewResponse(&adminv1.ListCredentialsResponse{Statuses: out}), nil
}

// validateToken asks the forge whether token is usable for host and maps
// its answer onto this repo's handler sentinels.
//
// The two forge sentinels stay distinct all the way to the wire, as
// forge/errors.go requires of this package by name: a token that does not
// authenticate is CodeInvalidArgument (the argument supplied is wrong), a
// token that authenticates but lacks write:repository is
// CodeFailedPrecondition (the argument is a real token; the state of the
// thing it names does not permit the operation). Neither is mapped to
// CodePermissionDenied, which is reserved here for requireAdmin -- folding
// the underscoped case in there would make "the CALLER is not an admin"
// and "the TOKEN is underscoped" indistinguishable on the wire, and one of
// those is the gate this package's own tests assert.
//
// Both sentinel branches DISCARD the underlying error rather than wrapping
// it. That is not laziness: the classification is the entire useful
// content, and the forge is a third party that could have echoed the
// submitted token back in its response body. Dropping the error removes
// that risk at the root instead of relying on redaction to catch it. The
// unclassified branch cannot drop it -- an operator debugging a broken
// forge needs the detail, and ErrorMapper LOGS this error before
// collapsing it to CodeInternal -- so that path is redacted instead.
func (h *Handler) validateToken(ctx context.Context, host, token string) error {
	err := h.validator.ValidateToken(ctx, host, token)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, forge.ErrInvalidToken):
		return fmt.Errorf("the forge at %s rejected the token: it does not authenticate (missing, malformed, expired, or revoked): %w", host, handler.ErrInvalidArgument)
	case errors.Is(err, forge.ErrInsufficientScope):
		return fmt.Errorf("the token authenticates against %s but lacks the write:repository scope needed to open pull requests: %w", host, handler.ErrFailedPrecondition)
	default:
		return fmt.Errorf("validating the token against %s: %w", host, redactErr(err, token))
	}
}

// requireAdmin is defence in depth on top of the routing-level gate, not a
// replacement for it: the whole /loam.admin.v1.* path group is already
// wrapped in httpauth.Auth.AdminOnly before any request reaches a handler
// (docs/web-spec.md -> Auth), which is why internal/handler/repoadmin
// documents having no per-RPC gate of its own.
//
// This package follows internal/handler/proposal in adding one anyway, and
// for a stronger version of proposal's reason. Its mutating RPC is the
// single most security-sensitive write in the system: it accepts a
// third-party forge credential in plaintext, and the token it stores is
// what every later push, PR, and fetch to that forge authenticates as. The
// read RPCs are gated too, because "which hosts is this server configured
// to reach, and are those credentials live?" is itself reconnaissance.
// httpauth.IsAdmin reads the flag AdminOnly itself sets, so this costs one
// context read and makes "only an admin can touch upstream credentials" a
// property asserted by this package's own tests rather than one inherited
// from a wiring line in cmd/server that no test in this package can see.
func requireAdmin(ctx context.Context, operation string) error {
	if httpauth.IsAdmin(ctx) {
		return nil
	}
	return fmt.Errorf("%s requires the admin superuser: %w", operation, handler.ErrPermissionDenied)
}

// redactToken returns s with every occurrence of token replaced by
// redactedMarker, the same construction internal/gittransport's
// scrubSecrets uses on git's output and argv. An empty token is a no-op:
// replacing "" would otherwise splice the marker between every rune.
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, redactedMarker)
}

// redactErr returns an error carrying err's message with every occurrence
// of token redacted, for the paths where the message must survive (it is
// logged by handler.ErrorMapper, which cannot know what a caller embedded
// in an error and deliberately does not try to redact).
//
// It intentionally does NOT wrap err: preserving the chain would preserve
// the unredacted message inside it, one Unwrap away from any caller and
// from any log line built with %+v. Nothing above this point matches
// anything in that chain with errors.Is -- the forge sentinels are
// classified before this is ever reached (see validateToken), and store
// errors reaching here are already unclassifiable -- so flattening costs
// no behaviour and closes the hole.
func redactErr(err error, token string) error {
	if err == nil {
		return nil
	}
	return errors.New(redactToken(err.Error(), token))
}

// statusToProto converts a store CredentialStatus to its proto form. The
// mapping is total and there is nothing else it could carry: the proto
// message has exactly three live fields (field 3, the retired SSH-key
// presence flag, is reserved), so no token can occupy it.
func statusToProto(status credentialstore.CredentialStatus) *adminv1.CredentialStatus {
	return &adminv1.CredentialStatus{
		Host:      status.Host,
		HasToken:  status.HasToken,
		Validated: status.Validated,
	}
}
