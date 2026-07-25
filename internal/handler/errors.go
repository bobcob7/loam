package handler

import (
	"errors"
	"log/slog"

	"connectrpc.com/connect"
)

// Domain error sentinels shared by every loam.v1 / loam.admin.v1 handler
// package. They are exported — despite the repo's usual "unexported unless
// needed externally" default — because handler packages live in separate
// Go packages (one per Connect service, per the interfaces.go convention
// in doc.go) and must be able to construct and wrap these same sentinels
// for (*ErrorMapper).ToConnectErr, defined once here, to recognize with
// errors.Is. An unexported sentinel cannot cross that package boundary.
var (
	// ErrNotFound indicates the requested resource does not exist.
	ErrNotFound = errors.New("handler: not found")
	// ErrAlreadyExists indicates a create collided with an existing resource.
	ErrAlreadyExists = errors.New("handler: already exists")
	// ErrFailedPrecondition indicates the request is valid but the
	// resource's current state does not permit it (e.g. removing a repo
	// with non-terminal work branches).
	ErrFailedPrecondition = errors.New("handler: failed precondition")
	// ErrInvalidArgument indicates malformed or missing request fields.
	ErrInvalidArgument = errors.New("handler: invalid argument")
	// ErrPermissionDenied indicates the caller lacks the capability the
	// RPC requires; see CapabilityChecker.
	ErrPermissionDenied = errors.New("handler: permission denied")
)

// ErrorMapper maps a handler's domain error to a Connect status code at
// the RPC boundary. It holds a logger (injected via NewErrorMapper) so an
// error it cannot classify is logged before being collapsed to
// CodeInternal — an unmapped error must never disappear silently.
type ErrorMapper struct {
	logger *slog.Logger
}

// NewErrorMapper builds an ErrorMapper that logs unmapped errors to logger.
func NewErrorMapper(logger *slog.Logger) *ErrorMapper {
	return &ErrorMapper{logger: logger}
}

// ToConnectErr maps err to a *connect.Error via errors.Is against the
// sentinels above, in the order documented on the bead: not found,
// already exists, failed precondition, invalid argument, permission
// denied. Any error matching none of them is logged at ERROR (the raw
// error, with no attempt to redact — this package does not know what
// callers embed in it) and returned as CodeInternal with a generic message
// so implementation detail never reaches the wire.
func (m *ErrorMapper) ToConnectErr(err error) *connect.Error {
	switch {
	case errors.Is(err, ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, ErrAlreadyExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, ErrFailedPrecondition):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, ErrPermissionDenied):
		return connect.NewError(connect.CodePermissionDenied, err)
	default:
		m.logger.Error("unmapped handler error", "error", err)
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}
