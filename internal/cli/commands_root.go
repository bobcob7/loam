package cli

import "context"

// runInstructions implements `loam instructions [command]` (see
// docs/cli-spec.md -> instructions). No flags; the single optional
// argument names the command to fetch focused help for instead of the full
// orientation.
func runInstructions(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("instructions")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) > 1 {
		return newUsageError("instructions takes at most one command argument")
	}
	return errNotImplemented
}

// runWhoami implements `loam whoami` (see docs/cli-spec.md -> whoami). No
// arguments, no flags.
func runWhoami(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("whoami")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) > 0 {
		return newUsageError("whoami takes no arguments")
	}
	return errNotImplemented
}

// runClone implements `loam clone <repo> <branch>` (see docs/cli-spec.md ->
// clone). No flags; both repo and branch are required — there is no
// default branch.
func runClone(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("clone")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) != 2 {
		return newUsageError("clone requires exactly a repo and a branch argument")
	}
	return runCloneCommand(ctx, deps, positional[0], positional[1])
}
