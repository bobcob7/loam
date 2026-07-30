package cli

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/pflag"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// newSearchFlags builds the pflag.FlagSet for `loam search <query> [--repo
// <repo>] [--all] [--limit <n>]` (see docs/cli-spec.md -> RAG queries
// (search)), plus the parsed --repo/--all/--limit values. --limit caps the
// number of returned chunks (default 10).
func newSearchFlags() (fs *pflag.FlagSet, repo *string, all *bool, limit *int) {
	fs = newFlagSet("search")
	repo = fs.String("repo", "", "target a specific enrolled repo")
	all = fs.Bool("all", false, "search across all enrolled repos")
	limit = fs.Int("limit", 10, "maximum number of chunks to return")
	return fs, repo, all, limit
}

// searchRow is search's row shape (docs/cli-spec.md -> RAG queries (search),
// "Output": `{ repo, file, lines: [40, 58], score: 0.82, snippet: ... }`).
type searchRow struct {
	Repo    string   `json:"repo"`
	File    string   `json:"file"`
	Lines   []uint32 `json:"lines"`
	Score   float32  `json:"score"`
	Snippet string   `json:"snippet"`
}

// searchRowsFrom converts the proto SearchResult list into search's row
// shape, packing start/end into the two-element `lines` pair the spec pins.
func searchRowsFrom(results []*loamv1.SearchResult) []searchRow {
	rows := make([]searchRow, 0, len(results))
	for _, r := range results {
		rows = append(rows, searchRow{
			Repo:    r.GetRepo(),
			File:    r.GetFile(),
			Lines:   []uint32{r.GetStartLine(), r.GetEndLine()},
			Score:   r.GetScore(),
			Snippet: r.GetSnippet(),
		})
	}
	return rows
}

// searchOutput is the {ingested, truncated, results} envelope `loam search`
// encodes (docs/cli-spec.md -> RAG queries (search), "Output": "the same
// envelope as graph"). It reuses graph's ingested conversion (graphIngestedFrom/
// graphIngestedOutput): SearchResponse.ingested is the same proto Ingested
// message QueryResponse.ingested carries, so the wire shape is identical --
// unlike graphQueryOutput's Results, which is typed any because multiple
// graph subqueries share the envelope, search has exactly one row shape, so
// Results is a concrete []searchRow here.
type searchOutput struct {
	Ingested  []graphIngestedOutput `json:"ingested"`
	Truncated bool                  `json:"truncated"`
	Results   []searchRow           `json:"results"`
}

// runSearch implements `loam search <query> [--repo <repo>] [--all]
// [--limit <n>]` (docs/cli-spec.md -> RAG queries (search)). query is
// required; --repo and --all are mutually exclusive scope selectors, same
// as graph (resolveGraphScope): --repo targets one enrolled repo, --all
// sends an empty QueryScope so the server fans out across every enrolled
// repo, and with neither flag the repo is inferred from the current
// directory. An unresolvable scope is a usage error (exit 2). This function
// only ever sends the scope the caller asked for -- repo-scope expansion
// (an empty QueryScope into concrete repos) is the server's job
// (handler.ScopeResolver), not this CLI's.
func runSearch(ctx context.Context, deps *Deps, args []string) error {
	fs, repo, all, limit := newSearchFlags()
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
	if *limit < 0 {
		return newUsageError("search: --limit must not be negative")
	}
	scope, err := resolveGraphScope(deps.workspace, *repo, *all)
	if err != nil {
		return err
	}
	req := &loamv1.SearchRequest{Query: positional[0], Scope: scope, Page: &loamv1.Page{Limit: uint32(*limit)}}
	resp, err := deps.connect.Search().Search(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("searching %q: %w", positional[0], err)
	}
	return deps.encoder.Encode(searchOutput{
		Ingested:  graphIngestedFrom(resp.Msg.GetIngested()),
		Truncated: resp.Msg.GetTruncated(),
		Results:   searchRowsFrom(resp.Msg.GetResults()),
	})
}
