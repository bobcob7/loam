package cli

import "context"

// runInstructions implements `loam instructions [command]` (see
// docs/cli-spec.md -> instructions). No flags; the optional trailing
// arguments name the command to fetch focused help for.
func runInstructions(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("instructions")
	if _, err := parseCommandArgs(fs, args); err != nil {
		return newUsageError(err.Error())
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

// runClone implements `loam clone <repo> [branch]` (see docs/cli-spec.md ->
// clone). No flags; repo is required.
func runClone(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("clone")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) < 1 {
		return newUsageError("clone requires a repo argument")
	}
	if len(positional) > 2 {
		return newUsageError("clone takes at most a repo and a branch")
	}
	return errNotImplemented
}
