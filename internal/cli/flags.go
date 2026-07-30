package cli

import (
	"io"

	"github.com/spf13/pflag"
)

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
		return nil, err
	}
	return fs.Args(), nil
}
