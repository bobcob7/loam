package cli

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/pflag"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// newGraphQueryFlags builds the pflag.FlagSet shared by every `loam graph
// <subquery> <target> [--repo <repo>] [--all] [--file <path>] [--limit
// <n>]` subquery (see docs/cli-spec.md -> Graph DB queries), plus the
// parsed --repo/--all/--file/--limit values. --file narrows an ambiguous
// symbol target to one file's definition; --limit caps result rows
// (default 50).
func newGraphQueryFlags(name string) (fs *pflag.FlagSet, repo *string, all *bool, file *string, limit *int) {
	fs = newFlagSet(name)
	repo = fs.String("repo", "", "target a specific enrolled repo")
	all = fs.Bool("all", false, "query across all enrolled repos")
	file = fs.String("file", "", "disambiguate the target to a specific file")
	limit = fs.Int("limit", 50, "maximum number of results to return")
	return fs, repo, all, file, limit
}

// graphQueryArgs is the parsed, validated shape shared by every graph
// subquery invocation: the single positional target, the resolved scope,
// the optional --file narrowing filter, and the Page built from --limit.
type graphQueryArgs struct {
	target string
	scope  *loamv1.QueryScope
	file   *string
	page   *loamv1.Page
}

// parseGraphQueryArgs parses and validates the shared flags/positional for
// one `loam graph <subquery> <target>` invocation (docs/cli-spec.md ->
// Graph DB queries): exactly one target argument, --repo and --all
// mutually exclusive, --limit non-negative, and scope resolved via ws when
// neither --repo nor --all is given.
func parseGraphQueryArgs(ws WorkspaceResolver, name string, args []string) (*graphQueryArgs, error) {
	fs, repo, all, file, limit := newGraphQueryFlags(name)
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return nil, newUsageError(err.Error())
	}
	if len(positional) != 1 {
		return nil, newUsageError(name + " requires exactly one target argument")
	}
	if *repo != "" && *all {
		return nil, newUsageError(name + ": --repo and --all are mutually exclusive")
	}
	if *limit < 0 {
		return nil, newUsageError(name + ": --limit must not be negative")
	}
	scope, err := resolveGraphScope(ws, *repo, *all)
	if err != nil {
		return nil, err
	}
	var filePtr *string
	if *file != "" {
		filePtr = file
	}
	return &graphQueryArgs{target: positional[0], scope: scope, file: filePtr, page: &loamv1.Page{Limit: uint32(*limit)}}, nil
}

// resolveGraphScope resolves the QueryScope for one graph subquery
// (docs/cli-spec.md -> Graph DB queries, "Scope"): --repo targets one
// enrolled repo, --all fans out across every enrolled repo (an empty
// QueryScope.Repos -- proto/loam/v1/common.proto's QueryScope doc comment:
// "The CLI only sends empty on explicit --all, so an accidental empty
// scope never fans out"), and with neither flag the repo is inferred from
// the current directory. An unresolvable scope is a usage error (exit 2,
// cli-spec: "If run outside a repo directory with neither flag, exit 2").
// The server, not this function, is responsible for expanding a --repo/
// inferred scope further -- this only ever sends exactly what the caller
// asked for.
func resolveGraphScope(ws WorkspaceResolver, repoFlag string, all bool) (*loamv1.QueryScope, error) {
	if repoFlag != "" {
		return &loamv1.QueryScope{Repos: []string{repoFlag}}, nil
	}
	if all {
		return &loamv1.QueryScope{}, nil
	}
	repo, err := ws.ResolveRepo()
	if err != nil {
		return nil, newUsageCLIError(fmt.Sprintf("cannot resolve scope: not inside a repo directory; pass --repo or --all: %s", err), err)
	}
	return &loamv1.QueryScope{Repos: []string{repo}}, nil
}

// graphMatchInfoOutput is the `of` sub-object docs/cli-spec.md's Ambiguity
// paragraph describes: present only when the query target was ambiguous
// over multiple candidates, naming which specific symbol/file/kind a row
// matched.
type graphMatchInfoOutput struct {
	Symbol string `json:"symbol"`
	File   string `json:"file"`
	Kind   string `json:"kind"`
}

// graphMatchInfoFrom converts a proto MatchInfo into its output shape, or
// nil when m is nil (the unambiguous case, so the row omits `of` entirely
// via its omitempty tag).
func graphMatchInfoFrom(m *loamv1.MatchInfo) *graphMatchInfoOutput {
	if m == nil {
		return nil
	}
	return &graphMatchInfoOutput{Symbol: m.GetSymbol(), File: m.GetFile(), Kind: m.GetKind()}
}

// graphIngestedOutput is one entry of the `ingested` envelope field (see
// docs/cli-spec.md -> Graph DB queries, "Output"): the commit each queried
// repo's index was built from, so a caller can judge how current the
// results are.
type graphIngestedOutput struct {
	Repo   string `json:"repo"`
	Target string `json:"target"`
	Ref    string `json:"ref"`
	At     string `json:"at"`
}

// graphIngestedFrom converts the proto Ingested list into its output shape.
func graphIngestedFrom(ingested []*loamv1.Ingested) []graphIngestedOutput {
	out := make([]graphIngestedOutput, 0, len(ingested))
	for _, in := range ingested {
		out = append(out, graphIngestedOutput{Repo: in.GetRepo(), Target: in.GetTarget(), Ref: in.GetRef(), At: in.GetAt()})
	}
	return out
}

// graphQueryOutput is the shared envelope every graph subquery encodes
// (docs/cli-spec.md -> Graph DB queries, "Output"): `results`' concrete
// element type varies per subquery (set via encodeGraphResult's caller),
// which is why Results is typed `any` here rather than a fixed row type.
type graphQueryOutput struct {
	Ingested  []graphIngestedOutput `json:"ingested"`
	Truncated bool                  `json:"truncated"`
	Results   any                   `json:"results"`
}

// encodeGraphResult builds and encodes the shared {ingested, truncated,
// results} envelope from a QueryResponse plus the subquery's own converted
// rows. It never inspects results' length to decide success/failure -- an
// empty slice here is exactly what an existing-but-unreferenced symbol
// looks like (see runGraphRefs), not a not-found condition; that
// distinction is the server's job (classifyConnectError handles a
// *connect.Error NotFound the server actually returns), never this
// function's.
func encodeGraphResult(deps *Deps, resp *loamv1.QueryResponse, results any) error {
	return deps.encoder.Encode(graphQueryOutput{
		Ingested:  graphIngestedFrom(resp.GetIngested()),
		Truncated: resp.GetTruncated(),
		Results:   results,
	})
}

// graphDefRow is `def`'s row shape (docs/cli-spec.md -> Graph DB queries,
// Result shapes table: `{ repo, file, line, symbol, kind }`).
type graphDefRow struct {
	Repo   string                `json:"repo"`
	File   string                `json:"file"`
	Line   uint32                `json:"line"`
	Symbol string                `json:"symbol"`
	Kind   string                `json:"kind"`
	Of     *graphMatchInfoOutput `json:"of,omitempty"`
}

// graphDefRowsFrom converts a LocationList's rows into def's row shape.
func graphDefRowsFrom(locations []*loamv1.Location) []graphDefRow {
	rows := make([]graphDefRow, 0, len(locations))
	for _, loc := range locations {
		rows = append(rows, graphDefRow{
			Repo:   loc.GetRepo(),
			File:   loc.GetFileLine().GetFile(),
			Line:   loc.GetFileLine().GetLine(),
			Symbol: loc.GetSymbol(),
			Kind:   loc.GetKind(),
			Of:     graphMatchInfoFrom(loc.GetOf()),
		})
	}
	return rows
}

// runGraphDef implements `loam graph def <symbol>` (docs/cli-spec.md ->
// Graph DB queries). A *connect.Error the server returns (NotFound for an
// undefined symbol) reaches mapCommandError unchanged via the %w wrap
// below, classifying to exit 3.
func runGraphDef(ctx context.Context, deps *Deps, args []string) error {
	qa, err := parseGraphQueryArgs(deps.workspace, "graph def", args)
	if err != nil {
		return err
	}
	req := &loamv1.QueryRequest{
		Scope: qa.scope,
		Page:  qa.page,
		File:  qa.file,
		Query: &loamv1.QueryRequest_Definition{Definition: &loamv1.DefinitionQuery{Symbol: qa.target}},
	}
	resp, err := deps.connect.Graph().Query(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("querying graph def %s: %w", qa.target, err)
	}
	return encodeGraphResult(deps, resp.Msg, graphDefRowsFrom(resp.Msg.GetLocations().GetLocations()))
}

// graphRefRow is `refs`' row shape (docs/cli-spec.md -> Graph DB queries,
// Result shapes table: `{ repo, file, line, symbol }` -- no `kind`, unlike
// `def`).
type graphRefRow struct {
	Repo   string                `json:"repo"`
	File   string                `json:"file"`
	Line   uint32                `json:"line"`
	Symbol string                `json:"symbol"`
	Of     *graphMatchInfoOutput `json:"of,omitempty"`
}

// graphRefRowsFrom converts a LocationList's rows into refs' row shape.
func graphRefRowsFrom(locations []*loamv1.Location) []graphRefRow {
	rows := make([]graphRefRow, 0, len(locations))
	for _, loc := range locations {
		rows = append(rows, graphRefRow{
			Repo:   loc.GetRepo(),
			File:   loc.GetFileLine().GetFile(),
			Line:   loc.GetFileLine().GetLine(),
			Symbol: loc.GetSymbol(),
			Of:     graphMatchInfoFrom(loc.GetOf()),
		})
	}
	return rows
}

// runGraphRefs implements `loam graph refs <symbol>` (docs/cli-spec.md ->
// Graph DB queries). IMPORTANT: an empty result here is NOT a not-found
// condition -- a real, defined symbol can legitimately have zero
// references. The server distinguishes "no such symbol" (a *connect.Error
// NotFound, which reaches mapCommandError via the %w wrap below and
// classifies to exit 3) from "symbol exists, nobody uses it" (a successful
// response with an empty LocationList, encoded here as `results: []` at
// exit 0) -- see loam-ofg.10's notes. This handler must never collapse
// that distinction by treating a short/empty rows slice as failure.
func runGraphRefs(ctx context.Context, deps *Deps, args []string) error {
	qa, err := parseGraphQueryArgs(deps.workspace, "graph refs", args)
	if err != nil {
		return err
	}
	req := &loamv1.QueryRequest{
		Scope: qa.scope,
		Page:  qa.page,
		File:  qa.file,
		Query: &loamv1.QueryRequest_References{References: &loamv1.ReferencesQuery{Symbol: qa.target}},
	}
	resp, err := deps.connect.Graph().Query(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("querying graph refs %s: %w", qa.target, err)
	}
	return encodeGraphResult(deps, resp.Msg, graphRefRowsFrom(resp.Msg.GetLocations().GetLocations()))
}

// graphDependencyRow is the shared row shape `deps` and `dependents` both
// use (docs/cli-spec.md -> Graph DB queries, Result shapes table:
// `{ repo, symbol, file, line, kind }`).
type graphDependencyRow struct {
	Repo   string                `json:"repo"`
	Symbol string                `json:"symbol"`
	File   string                `json:"file"`
	Line   uint32                `json:"line"`
	Kind   string                `json:"kind"`
	Of     *graphMatchInfoOutput `json:"of,omitempty"`
}

// graphDependencyRowFrom converts one endpoint Location of a
// DependencyEdge into the shared dependency row shape.
func graphDependencyRowFrom(loc *loamv1.Location) graphDependencyRow {
	return graphDependencyRow{
		Repo:   loc.GetRepo(),
		Symbol: loc.GetSymbol(),
		File:   loc.GetFileLine().GetFile(),
		Line:   loc.GetFileLine().GetLine(),
		Kind:   loc.GetKind(),
		Of:     graphMatchInfoFrom(loc.GetOf()),
	}
}

// graphDepsRowsFrom converts a DependencyList into deps' rows, using each
// edge's "to" endpoint -- the depended-upon side, i.e. what the target
// depends on (see proto/loam/v1/graph.proto's DependencyEdge: "to: The
// depended-upon endpoint").
func graphDepsRowsFrom(edges []*loamv1.DependencyEdge) []graphDependencyRow {
	rows := make([]graphDependencyRow, 0, len(edges))
	for _, edge := range edges {
		rows = append(rows, graphDependencyRowFrom(edge.GetTo()))
	}
	return rows
}

// runGraphDeps implements `loam graph deps <file|symbol>` (docs/cli-spec.md
// -> Graph DB queries).
func runGraphDeps(ctx context.Context, deps *Deps, args []string) error {
	qa, err := parseGraphQueryArgs(deps.workspace, "graph deps", args)
	if err != nil {
		return err
	}
	req := &loamv1.QueryRequest{
		Scope: qa.scope,
		Page:  qa.page,
		File:  qa.file,
		Query: &loamv1.QueryRequest_Dependencies{Dependencies: &loamv1.DependenciesQuery{Target: qa.target}},
	}
	resp, err := deps.connect.Graph().Query(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("querying graph deps %s: %w", qa.target, err)
	}
	return encodeGraphResult(deps, resp.Msg, graphDepsRowsFrom(resp.Msg.GetDependencies().GetEdges()))
}

// graphDependentsRowsFrom converts a DependencyList into dependents' rows,
// using each edge's "from" endpoint -- the dependent side, i.e. what has
// the dependency on the target (blast radius; see
// proto/loam/v1/graph.proto's DependencyEdge: "from: The dependent
// endpoint").
func graphDependentsRowsFrom(edges []*loamv1.DependencyEdge) []graphDependencyRow {
	rows := make([]graphDependencyRow, 0, len(edges))
	for _, edge := range edges {
		rows = append(rows, graphDependencyRowFrom(edge.GetFrom()))
	}
	return rows
}

// runGraphDependents implements `loam graph dependents <file|symbol>`
// (docs/cli-spec.md -> Graph DB queries).
func runGraphDependents(ctx context.Context, deps *Deps, args []string) error {
	qa, err := parseGraphQueryArgs(deps.workspace, "graph dependents", args)
	if err != nil {
		return err
	}
	req := &loamv1.QueryRequest{
		Scope: qa.scope,
		Page:  qa.page,
		File:  qa.file,
		Query: &loamv1.QueryRequest_Dependents{Dependents: &loamv1.DependentsQuery{Target: qa.target}},
	}
	resp, err := deps.connect.Graph().Query(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("querying graph dependents %s: %w", qa.target, err)
	}
	return encodeGraphResult(deps, resp.Msg, graphDependentsRowsFrom(resp.Msg.GetDependencies().GetEdges()))
}

// graphHistoryRow is `history`'s row shape (docs/cli-spec.md -> Graph DB
// queries, Result shapes table: `{ repo, symbol, file, commit, ref,
// message }`).
//
// KNOWN PROTO GAP (reported, not fixed here -- this bead stays inside
// internal/cli, and loam-ofg.10 is coding the server against the same
// contract right now): proto/loam/v1/graph.proto's HistoryEntry carries
// only repo/commit/ref/message, with no symbol, file, or `of` field, unlike
// Location. Symbol is populated here from the query's own target argument
// -- already known locally, not fabricated -- but File has no data source
// at all and is always "", and an ambiguous history target cannot surface
// an `of` disambiguator the way def/refs/deps/dependents do. Flagged for
// the maintainer to either extend HistoryEntry or confirm this gap is
// accepted (see loam-0pj.14's report).
type graphHistoryRow struct {
	Repo    string                `json:"repo"`
	Symbol  string                `json:"symbol"`
	File    string                `json:"file"`
	Commit  string                `json:"commit"`
	Ref     string                `json:"ref"`
	Message string                `json:"message"`
	Of      *graphMatchInfoOutput `json:"of,omitempty"`
}

// graphHistoryRowsFrom converts a HistoryList into history's rows. symbol
// is the query's own target argument (see graphHistoryRow's doc comment).
func graphHistoryRowsFrom(entries []*loamv1.HistoryEntry, symbol string) []graphHistoryRow {
	rows := make([]graphHistoryRow, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, graphHistoryRow{
			Repo:    entry.GetRepo(),
			Symbol:  symbol,
			Commit:  entry.GetCommit(),
			Ref:     entry.GetRef(),
			Message: entry.GetMessage(),
		})
	}
	return rows
}

// runGraphHistory implements `loam graph history <symbol>`
// (docs/cli-spec.md -> Graph DB queries).
func runGraphHistory(ctx context.Context, deps *Deps, args []string) error {
	qa, err := parseGraphQueryArgs(deps.workspace, "graph history", args)
	if err != nil {
		return err
	}
	req := &loamv1.QueryRequest{
		Scope: qa.scope,
		Page:  qa.page,
		File:  qa.file,
		Query: &loamv1.QueryRequest_History{History: &loamv1.HistoryQuery{Symbol: qa.target}},
	}
	resp, err := deps.connect.Graph().Query(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("querying graph history %s: %w", qa.target, err)
	}
	return encodeGraphResult(deps, resp.Msg, graphHistoryRowsFrom(resp.Msg.GetHistory().GetEntries(), qa.target))
}
