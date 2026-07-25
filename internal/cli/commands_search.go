package cli

import (
	"context"
	"flag"
)

// newSearchFlags builds the flag.FlagSet for `loam search <query> [--repo
// <repo>] [--all] [--limit <n>]` (see docs/cli-spec.md -> RAG queries),
// plus the parsed --repo/--all values.
func newSearchFlags() (fs *flag.FlagSet, repo *string, all *bool) {
	fs = newFlagSet("search")
	repo = fs.String("repo", "", "target a specific enrolled repo")
	all = fs.Bool("all", false, "search across all enrolled repos")
	fs.Int("limit", 10, "maximum number of chunks to return")
	return fs, repo, all
}

// runSearch implements `loam search <query> [--repo <repo>] [--all]
// [--limit <n>]`. query is required; --repo and --all are mutually
// exclusive scope selectors, same as graph.
func runSearch(ctx context.Context, deps *Deps, args []string) error {
	fs, repo, all := newSearchFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) != 1 {
		return newUsageError("search requires exactly one query argument")
	}
	if *repo != "" && *all {
		return newUsageError("search: --repo and --all are mutually exclusive")
	}
	return errNotImplemented
}
