package cli

import (
	"errors"
	"io"

	"github.com/spf13/pflag"
)

// errHelpRequested replaces pflag.ErrHelp at the parse site (see
// parseCommandArgs) so a caller's `return newUsageError(err.Error())` never
// surfaces pflag's own internal sentinel text ("pflag: help requested") to
// an agent (loam-q0ek). help.go's TryHelp is the actual fix -- it
// recognizes every documented `-h`/`--help` route from argv alone, before
// main() ever builds a Deps or dispatches to a handler, so a handler's own
// parseCommandArgs call is not normally reached with a help flag at all in
// the compiled binary. This sentinel exists purely as defense in depth for
// any other caller that reaches a handler directly (e.g. a test dispatching
// the Router without going through TryHelp first): it is a plain sentinel
// error rather than a *cliError itself because parseCommandArgs's contract
// is unchanged -- every one of its 16 callers still does its own
// `newUsageError(err.Error())` -- only what err.Error() reads has changed.
var errHelpRequested = errors.New(`help requested; run "loam <command> --help" (or "loam help") for usage`)

// newFlagSet builds a per-command pflag.FlagSet. It never touches the global
// pflag.CommandLine set, and it never prints its own usage or exits the
// process — callers turn a parse failure into a usageError instead.
//
// pflag.FlagSet defaults SetInterspersed to true, so flags may appear
// anywhere among positional arguments — matching synopses in
// docs/cli-spec.md that place flags after positionals (e.g. "work set
// [repo] [work-branch] [--title <title>]"). That default is left as-is
// here rather than restated, since flipping it is the one thing this
// function must never do.
func newFlagSet(name string) *pflag.FlagSet {
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseCommandArgs parses args with fs and returns the positional tokens
// pflag left over in fs.Args(). Handlers call this instead of fs.Parse
// directly only for symmetry with returning positionals alongside the
// error; pflag itself does the interspersed flag/positional partitioning
// and the "--" handling that this package used to hand-rolled in a
// bespoke splitArgs helper.
func parseCommandArgs(fs *pflag.FlagSet, args []string) ([]string, error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil, errHelpRequested
		}
		return nil, err
	}
	return fs.Args(), nil
}
