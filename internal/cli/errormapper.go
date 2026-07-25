package cli

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"
)

// Error code strings (see docs/cli-spec.md -> Exit Codes & Errors). Each
// names the specific failure within its exit-code class: usage,
// unauthorized, conflict, and precondition_failed all share exit 2;
// not_found is exit 3 on its own.
const (
	codeUsage              = "usage"
	codeUnauthorized       = "unauthorized"
	codeConflict           = "conflict"
	codePreconditionFailed = "precondition_failed"
	codeNotFound           = "not_found"
	codeInternal           = "internal"
)

// Unexported sentinel errors identifying each error class. Command handlers
// (and tests) match against these with errors.Is; cliError.Unwrap exposes
// the one each instance was built from.
var (
	errMissingEnv         = errors.New("missing required environment variable")
	errMalformedEnv       = errors.New("malformed environment variable")
	errUsage              = errors.New("usage error")
	errUnauthorized       = errors.New("unauthorized")
	errConflict           = errors.New("conflict")
	errPreconditionFailed = errors.New("precondition failed")
	errNotFound           = errors.New("not found")
)

// cliError is a structured command-level error: a code naming its
// exit-code class (see docs/cli-spec.md -> Exit Codes & Errors) plus a
// human-readable message. It wraps a sentinel (and optionally an
// underlying cause) so callers can match it with errors.Is/errors.As
// instead of a bare type assertion.
type cliError struct {
	code    string
	message string
	cause   error // the raw external cause, if any; nil when there is none
	unwrap  error // sentinel, or sentinel-plus-cause, for errors.Is/errors.As
}

// Error returns the message, plus the external cause's own message when
// one was given (e.g. the underlying Connect error mapped at the handler
// boundary) — never the internal classification sentinel, which exists
// only for errors.Is matching.
func (e *cliError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s", e.message, e.cause.Error())
	}
	return e.message
}

// Unwrap exposes the sentinel (and cause, when present) so errors.Is and
// errors.As traverse into either.
func (e *cliError) Unwrap() error { return e.unwrap }

// newCLIError builds a cliError of the given code, wrapping sentinel (one
// of the errFoo sentinels above) so errors.Is(err, sentinel) matches. cause
// is the original error being classified (e.g. a *connect.Error), or nil
// when there is no separate underlying cause. Wrapping both sentinel and
// cause with a single fmt.Errorf("%w: %w", ...) — Go's multi-error
// wrapping — lets errors.Is/errors.As reach either one.
func newCLIError(code, message string, sentinel, cause error) *cliError {
	unwrap := sentinel
	if cause != nil && !errors.Is(cause, sentinel) {
		unwrap = fmt.Errorf("%w: %w", sentinel, cause)
	}
	return &cliError{code: code, message: message, cause: cause, unwrap: unwrap}
}

// newUsageCLIError builds a command-level usage error (exit 2, code
// "usage") — distinct from the router-level usageError in errors.go, which
// covers flag-parsing/dispatch failures independent of the ErrorMapper.
func newUsageCLIError(message string, cause error) *cliError {
	return newCLIError(codeUsage, message, errUsage, cause)
}

// newUnauthorizedError builds an authorization-denied error (exit 2, code
// "unauthorized").
func newUnauthorizedError(message string, cause error) *cliError {
	return newCLIError(codeUnauthorized, message, errUnauthorized, cause)
}

// newConflictError builds a conflict error (exit 2, code "conflict").
func newConflictError(message string, cause error) *cliError {
	return newCLIError(codeConflict, message, errConflict, cause)
}

// newPreconditionFailedError builds a precondition-failed error (exit 2,
// code "precondition_failed") — e.g. a work-branch state gate violation.
func newPreconditionFailedError(message string, cause error) *cliError {
	return newCLIError(codePreconditionFailed, message, errPreconditionFailed, cause)
}

// newNotFoundError builds a not-found error (exit 3, code "not_found").
func newNotFoundError(message string, cause error) *cliError {
	return newCLIError(codeNotFound, message, errNotFound, cause)
}

// exitCodeForClass maps an error code (one of the codeFoo constants above)
// to its exit-code class (see docs/cli-spec.md -> Exit Codes & Errors).
// Anything unrecognized is treated as an unexpected internal error.
func exitCodeForClass(code string) int {
	switch code {
	case codeNotFound:
		return 3
	case codeUsage, codeUnauthorized, codeConflict, codePreconditionFailed:
		return 2
	default:
		return 1
	}
}

// classifyConnectError maps a ConnectRPC status code to this CLI's error
// classes (see docs/cli-spec.md -> Exit Codes & Errors design notes):
// NotFound -> not_found; PermissionDenied/Unauthenticated -> unauthorized;
// FailedPrecondition -> precondition_failed; AlreadyExists/Aborted ->
// conflict; InvalidArgument -> usage; anything else is unexpected (exit 1).
func classifyConnectError(err *connect.Error) *cliError {
	switch err.Code() {
	case connect.CodeNotFound:
		return newNotFoundError(err.Message(), err)
	case connect.CodePermissionDenied, connect.CodeUnauthenticated:
		return newUnauthorizedError(err.Message(), err)
	case connect.CodeFailedPrecondition:
		return newPreconditionFailedError(err.Message(), err)
	case connect.CodeAlreadyExists, connect.CodeAborted:
		return newConflictError(err.Message(), err)
	case connect.CodeInvalidArgument:
		return newUsageCLIError(err.Message(), err)
	default:
		return nil
	}
}

// mapCommandError classifies err at the command boundary into a *cliError,
// recognizing an error already carrying a cliError/usageError classification
// as well as a raw *connect.Error a handler forgot to map itself. Returns
// nil when err does not resolve to a known class (an unexpected internal
// error, exit 1).
func mapCommandError(err error) *cliError {
	var ce *cliError
	if errors.As(err, &ce) {
		return ce
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return newUsageCLIError(ue.Error(), ue)
	}
	var connErr *connect.Error
	if errors.As(err, &connErr) {
		return classifyConnectError(connErr)
	}
	return nil
}

// cliErrorMapper implements ErrorMapper by classifying err via
// mapCommandError and mapping the resulting class to its exit code; an
// error that resolves to no known class is an unexpected internal error
// (exit 1). Replaces the coarseErrorMapper placeholder, which collapsed
// every error to exit 1.
type cliErrorMapper struct{}

// newErrorMapper constructs the real ErrorMapper.
func newErrorMapper() *cliErrorMapper { return &cliErrorMapper{} }

// ExitCode returns 0 for a nil error, otherwise the exit-code class of err
// per docs/cli-spec.md -> Exit Codes & Errors.
func (m *cliErrorMapper) ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if ce := mapCommandError(err); ce != nil {
		return exitCodeForClass(ce.code)
	}
	return 1
}
