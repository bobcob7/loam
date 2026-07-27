package graph

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/codegraph"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
)

// scopedSymbol pairs a resolved codegraph.Symbol with the ScopedRepo it was
// found in, so callers can attribute a Location/DependencyEdge/HistoryEntry
// back to the right repo name and indexed branch after resolveSymbols' per-
// repo fan-out.
type scopedSymbol struct {
	repo   handler.ScopedRepo
	symbol codegraph.Symbol
}

// resolveSymbols runs LookupSymbolsByName once per repo in scoped -- the
// `--all` fan-out docs/cli-spec.md:553-557 describes ("queries each repo's
// graph independently and unions the results") -- and merges the matches.
// This is deliberately NOT a single call over every repoID at once:
// internal/codegraph.Store.LookupSymbolsByName's single targetBranch
// parameter assumes every repoID in one call shares that branch, but
// distinct repos can have distinct indexed_branch values
// (docs/persistence-spec.md "repos"). Calling it once per repo, with that
// repo's own indexed branch, is correct regardless of whether repos in
// scope happen to share a branch name.
//
// mergeLimit bounds the MERGED result across every repo (each repo is asked
// for up to mergeLimit rows in isolation, then the merged set is itself
// trimmed to mergeLimit) -- not a per-repo cap in isolation. truncated
// reports whether any per-repo call, or the final merge trim, cut a real
// match.
func (h *Handler) resolveSymbols(ctx context.Context, scoped []handler.ScopedRepo, name, file string, mergeLimit int32) ([]scopedSymbol, bool, error) {
	var all []scopedSymbol
	var truncated bool
	for _, repo := range scoped {
		symbols, repoTruncated, err := h.symbols.LookupSymbolsByName(ctx, []uuid.UUID{repo.ID}, repo.IndexedBranch, name, file, mergeLimit)
		if err != nil {
			return nil, false, fmt.Errorf("looking up %q in repo %s: %w", name, repo.Name, err)
		}
		truncated = truncated || repoTruncated
		for _, s := range symbols {
			all = append(all, scopedSymbol{repo: repo, symbol: s})
		}
	}
	if int32(len(all)) > mergeLimit {
		all = all[:mergeLimit]
		truncated = true
	}
	return all, truncated, nil
}

// symbolExists reports whether name resolves to at least one symbol
// anywhere in scoped, without caring how many or fetching more than one row
// per repo -- `graph refs`'s step-one existence check (see queryReferences'
// doc comment): its own result is discarded once known non-empty, so a
// small per-repo limit of 1 is deliberate, not an oversight.
func (h *Handler) symbolExists(ctx context.Context, scoped []handler.ScopedRepo, name, file string) (bool, error) {
	for _, repo := range scoped {
		symbols, _, err := h.symbols.LookupSymbolsByName(ctx, []uuid.UUID{repo.ID}, repo.IndexedBranch, name, file, 1)
		if err != nil {
			return false, fmt.Errorf("checking existence of %q in repo %s: %w", name, repo.Name, err)
		}
		if len(symbols) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// toLocation converts a resolved codegraph.Symbol to the wire Location
// shape. ambiguous marks whether the target this symbol was matched against
// had more than one candidate (docs/cli-spec.md:528-533's "of" disambiguation
// field: "present only when the target was ambiguous"); includeKind
// controls whether Kind is copied through -- false only for
// toLocationFromReference's refs rows, whose documented shape
// (docs/cli-spec.md:544) omits it (see codegraph.Reference's own doc
// comment).
func toLocation(repoName string, s codegraph.Symbol, ambiguous, includeKind bool) *loamv1.Location {
	name := s.Name
	loc := &loamv1.Location{Repo: repoName, FileLine: &loamv1.FileLine{File: s.File}, Symbol: &name}
	if s.Line != nil {
		line := uint32(*s.Line)
		loc.FileLine.Line = &line
	}
	if includeKind {
		loc.Kind = s.Kind
	}
	if ambiguous {
		loc.Of = &loamv1.MatchInfo{Symbol: s.Name, File: s.File, Kind: s.Kind}
	}
	return loc
}

// toLocationFromReference converts a codegraph.Reference (a `graph refs`
// use site) to the wire Location shape. Kind is deliberately left at its
// proto zero value: docs/cli-spec.md:544's refs row shape
// (`{ repo, file, line, symbol }`) has no kind field, unlike def/deps/
// dependents, per codegraph.Reference's own doc comment -- an unset Kind
// here is what achieves that omission. Of is never set: unlike def/deps/
// dependents, a reference row is not itself one of several candidate
// matches for an ambiguous target -- LookupReferencesByName returns every
// reference to name directly, with no per-candidate-symbol fan-out to
// disambiguate against.
func toLocationFromReference(repoName string, r codegraph.Reference) *loamv1.Location {
	name := r.Name
	line := uint32(r.Line)
	return &loamv1.Location{Repo: repoName, FileLine: &loamv1.FileLine{File: r.File, Line: &line}, Symbol: &name}
}

// toProtoIngested converts handler.Ingested entries to the wire shape.
func toProtoIngested(entries []handler.Ingested) []*loamv1.Ingested {
	result := make([]*loamv1.Ingested, len(entries))
	for i, e := range entries {
		result[i] = &loamv1.Ingested{Repo: e.Repo, Target: e.Target, Ref: e.Ref, At: e.At}
	}
	return result
}
