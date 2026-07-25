package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// newFlagSet builds a per-command flag.FlagSet. It never touches the global
// flag.CommandLine set, and it never prints its own usage or exits the
// process — callers turn a parse failure into a usageError instead.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseCommandArgs splits args into flag tokens and positional tokens via
// splitArgs, parses the flag tokens with fs, and returns the positional
// tokens. Handlers call this instead of fs.Parse directly because several
// synopses in docs/cli-spec.md place flags after positionals (e.g. "work
// set [repo] [work-branch] [--title <title>]"), which fs.Parse alone
// cannot handle: it stops scanning at the first non-flag token.
func parseCommandArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	flagArgs, positional, err := splitArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}
	return positional, nil
}

// splitArgs partitions args into the flag tokens (each flag immediately
// followed by its value token when it takes one, in original relative
// order) and the positional tokens — everything else. fs.Parse is only
// ever handed the flag tokens; parseCommandArgs returns the positional
// tokens itself rather than relying on fs.Args().
//
// This sidesteps two accidents of feeding a naively reordered
// "flags-then-positionals" slice straight to fs.Parse: it stops at the
// first non-flag token (so a positional preceding a flag would hide
// everything after it from fs.Parse, including later flags), and it only
// recognizes "--" as a terminator when nothing unparsed precedes it (so
// reordering a "--" next to hoisted flags can silently invert or defeat
// its meaning).
//
// "--" itself ends flag scanning immediately here: it is dropped, and
// every token after it — even one that looks like a flag — is taken as a
// literal positional. A recognized non-boolean flag with no following
// token is a usage error ("flag needs an argument"), never a silent
// reinterpretation of the next positional as that flag's value.
func splitArgs(fs *flag.FlagSet, args []string) (flagArgs, positional []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.ContainsRune(name, '=') {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		if boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && boolFlag.IsBoolFlag() {
			continue
		}
		if i+1 >= len(args) {
			return nil, nil, fmt.Errorf("flag needs an argument: -%s", name)
		}
		i++
		flagArgs = append(flagArgs, args[i])
	}
	return flagArgs, positional, nil
}
