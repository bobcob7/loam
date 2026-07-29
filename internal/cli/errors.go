package cli

// errNotImplemented is deliberately gone. It was the placeholder every
// command handler returned while the command beads were outstanding; with
// `instructions` and `whoami` real (loam-0pj.7), every leaf in
// commandTree() has a real handler and nothing returns it any more. Keeping
// an unreferenced "not implemented" sentinel around would invite a future
// handler to reach for it instead of returning a classified *cliError.

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
