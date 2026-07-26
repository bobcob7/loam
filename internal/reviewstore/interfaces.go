// Package reviewstore implements review_rounds and verdicts against
// Postgres via the sqlc-generated queries in internal/db/gen
// (docs/persistence-spec.md "review_rounds", "verdicts"). RoundStore owns
// opening and reading review rounds; VerdictStore owns submitting and
// listing verdicts and the current-round approve count.
//
// Staleness is DERIVED, never stored: a verdict is stale iff its round is
// not the work branch's current round (the review_rounds row with the
// highest number for that branch). There is no stale column, no partial
// index, and no MarkStale method anywhere in this package or its
// migration -- every read that needs "is this current" recomputes it, by
// comparing round numbers, at query time.
package reviewstore

import (
	"github.com/bobcob7/loam/internal/db/gen"
)

//go:generate go tool moq -out moq_test.go . querier

// querier is the minimal Postgres surface RoundStore and VerdictStore
// need: sqlc's generated DBTX. Defined here at the consumer, per repo
// convention, so both stores can be unit-tested against a moq mock
// instead of a live pool; *pgxpool.Pool satisfies it in production
// without modification.
type querier interface {
	gen.DBTX
}
