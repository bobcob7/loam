package cli

import "context"

// runSearch implements `loam search <query> [--repo <repo>] [--all]
// [--limit <n>]` (see docs/cli-spec.md -> RAG queries). query is required;
// --repo and --all are mutually exclusive scope selectors, same as graph.
func runSearch(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("search")
	repo := fs.String("repo", "", "target a specific enrolled repo")
	all := fs.Bool("all", false, "search across all enrolled repos")
	fs.Int("limit", 10, "maximum number of chunks to return")
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
