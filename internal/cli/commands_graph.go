package cli

import "context"

// runGraphQuery implements the shared shape of every `loam graph <subquery>
// <target> [--repo <repo>] [--all] [--file <path>] [--limit <n>]` subquery
// (see docs/cli-spec.md -> Graph DB queries). --file and --limit are the
// NOTES spec correction on top of the documented synopsis. --repo and --all
// are mutually exclusive scope selectors; with neither, scope is inferred
// from the current directory (a later bead, via WorkspaceResolver).
func runGraphQuery(name string, deps *Deps, args []string) error {
	fs := newFlagSet(name)
	repo := fs.String("repo", "", "target a specific enrolled repo")
	all := fs.Bool("all", false, "query across all enrolled repos")
	fs.String("file", "", "disambiguate the target to a specific file")
	fs.Int("limit", 0, "maximum number of results to return")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) != 1 {
		return newUsageError(name + " requires exactly one target argument")
	}
	if *repo != "" && *all {
		return newUsageError(name + ": --repo and --all are mutually exclusive")
	}
	return errNotImplemented
}

// runGraphDef implements `loam graph def <symbol>`.
func runGraphDef(ctx context.Context, deps *Deps, args []string) error {
	return runGraphQuery("graph def", deps, args)
}

// runGraphRefs implements `loam graph refs <symbol>`.
func runGraphRefs(ctx context.Context, deps *Deps, args []string) error {
	return runGraphQuery("graph refs", deps, args)
}

// runGraphDeps implements `loam graph deps <file|symbol>`.
func runGraphDeps(ctx context.Context, deps *Deps, args []string) error {
	return runGraphQuery("graph deps", deps, args)
}

// runGraphDependents implements `loam graph dependents <file|symbol>`.
func runGraphDependents(ctx context.Context, deps *Deps, args []string) error {
	return runGraphQuery("graph dependents", deps, args)
}

// runGraphHistory implements `loam graph history <symbol>`.
func runGraphHistory(ctx context.Context, deps *Deps, args []string) error {
	return runGraphQuery("graph history", deps, args)
}
