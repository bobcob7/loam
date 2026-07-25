package cli

import "context"

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
	if ue, ok := err.(*usageError); ok {
		_ = router.deps.Encoder.Encode(errorPayload{Error: errorDetail{Code: "usage", Message: ue.Error()}})
		return exitUsage
	}
	_ = router.deps.Encoder.Encode(errorPayload{Error: errorDetail{Code: "internal", Message: err.Error()}})
	return router.deps.Errors.ExitCode(err)
}
