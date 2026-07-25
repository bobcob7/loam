package cli

import (
	"flag"
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

// parseCommandArgs reorders args so every flag registered on fs precedes
// positional arguments, then parses it. The stdlib flag package stops
// scanning at the first non-flag token, but several synopses in
// docs/cli-spec.md place flags after positionals (e.g. "work set [repo]
// [work-branch] [--title <title>]"), so handlers call this instead of
// fs.Parse directly.
func parseCommandArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	reordered := reorderArgs(fs, args)
	if err := fs.Parse(reordered); err != nil {
		return nil, err
	}
	return fs.Args(), nil
}

// reorderArgs walks args, moving every token fs recognizes as a flag (and
// its value, if the flag takes one) to the front, and returns everything
// else — the positionals — after them, preserving relative order within
// each group.
func reorderArgs(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-" || arg == "--" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
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
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}
