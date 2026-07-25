package cli

import "errors"

// errNotImplemented is returned by every command handler at this stage.
// Later beads replace individual handler bodies with real behavior.
var errNotImplemented = errors.New("not implemented")

// usageError is a structured usage/routing error: no command given, an
// unknown command or subcommand, or a per-command flag parse failure. It
// always maps to exit code 2 (see docs/cli-spec.md -> Exit Codes & Errors),
// independent of the injected ErrorMapper, which governs command-level
// errors instead.
type usageError struct{ message string }

// newUsageError builds a usageError with the given message.
func newUsageError(message string) *usageError { return &usageError{message: message} }

func (e *usageError) Error() string { return e.message }

// errorPayload is the structured error shape written to the active output
// format (see docs/cli-spec.md -> Exit Codes & Errors).
type errorPayload struct {
	Error errorDetail `json:"error"`
}

// errorDetail carries the failure's code and human-readable message.
type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
