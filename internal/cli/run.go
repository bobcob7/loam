package cli

import (
	"context"
	"errors"
)

// exitUsage is the fixed exit code for routing/usage failures (see
// docs/cli-spec.md -> Exit Codes & Errors). It is not delegated to the
// injected ErrorMapper: usage errors are a router concern, ErrorMapper is a
// command-execution concern owned by loam-0pj.4.
const exitUsage = 2

// Run dispatches args through router and encodes the result (or structured
// error) via the router's injected OutputEncoder, returning the process
// exit code. main() is expected to call os.Exit with the return value.
func Run(ctx context.Context, router *Router, args []string) int {
	err := router.Dispatch(ctx, args)
	if err == nil {
		return 0
	}
	var ue *usageError
	if errors.As(err, &ue) {
		_ = router.deps.encoder.Encode(errorPayload{Error: errorDetail{Code: codeUsage, Message: ue.Error()}})
		return exitUsage
	}
	code := codeInternal
	message := err.Error()
	if ce := mapCommandError(err); ce != nil {
		// ce.Error() is the classified message alone (e.g. a *connect.Error's
		// own Message()); err.Error() on the raw error can be something else
		// entirely — a *connect.Error's Error() prepends its own code string
		// ("not_found: work branch wb-1 not found"), which would otherwise
		// duplicate the code already carried separately in errorDetail.Code.
		code = ce.code
		message = ce.Error()
	}
	_ = router.deps.encoder.Encode(errorPayload{Error: errorDetail{Code: code, Message: message}})
	return router.deps.errorMapper.ExitCode(err)
}
