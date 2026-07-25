package cli

import (
	"context"
	"flag"
)

// newGraphQueryFlags builds the flag.FlagSet shared by every `loam graph
// <subquery> <target> [--repo <repo>] [--all] [--file <path>] [--limit
// <n>]` subquery (see docs/cli-spec.md -> Graph DB queries), plus the
// parsed --repo/--all values. --file narrows an ambiguous symbol target to
// one file's definition; --limit caps result rows (default 50).
func newGraphQueryFlags(name string) (fs *flag.FlagSet, repo *string, all *bool) {
	fs = newFlagSet(name)
	repo = fs.String("repo", "", "target a specific enrolled repo")
	all = fs.Bool("all", false, "query across all enrolled repos")
	fs.String("file", "", "disambiguate the target to a specific file")
	fs.Int("limit", 50, "maximum number of results to return")
	return fs, repo, all
}

// runGraphQuery implements the shared shape of every graph subquery.
// --repo and --all are mutually exclusive scope selectors; with neither,
// scope is inferred from the current directory (a later bead, via
// WorkspaceResolver).
func runGraphQuery(ctx context.Context, name string, deps *Deps, args []string) error {
	fs, repo, all := newGraphQueryFlags(name)
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
	return runGraphQuery(ctx, "graph def", deps, args)
}

// runGraphRefs implements `loam graph refs <symbol>`.
func runGraphRefs(ctx context.Context, deps *Deps, args []string) error {
	return runGraphQuery(ctx, "graph refs", deps, args)
}

// runGraphDeps implements `loam graph deps <file|symbol>`.
func runGraphDeps(ctx context.Context, deps *Deps, args []string) error {
	return runGraphQuery(ctx, "graph deps", deps, args)
}

// runGraphDependents implements `loam graph dependents <file|symbol>`.
func runGraphDependents(ctx context.Context, deps *Deps, args []string) error {
	return runGraphQuery(ctx, "graph dependents", deps, args)
}

// runGraphHistory implements `loam graph history <symbol>`.
func runGraphHistory(ctx context.Context, deps *Deps, args []string) error {
	return runGraphQuery(ctx, "graph history", deps, args)
}
