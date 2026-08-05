// Package ingestjobs implements the ingest_jobs store: the worker queue
// state backing the ingestion pipeline's trigger/scheduling loop
// (docs/ingestion-spec.md "Trigger & Scheduling") and the web Jobs view
// (docs/web-spec.md -> RepoAdminService.ListIngestJobs;
// docs/persistence-spec.md "ingest_jobs").
//
// It provides the queue's write surface (Enqueue, Claim, Complete, Fail,
// Requeue) and its reads (Get, List), enforcing at most one running job
// per repo at claim time (Store.Claim's doc comment) and the
// queued -> running -> succeeded|failed lifecycle as guarded UPDATEs, the
// same shape internal/workbranchstore and internal/rolestore use for their
// own transitions.
//
// internal/ingest.Pool already owns a separate, hand-written-SQL path
// against this same table (its own claim/succeed/fail/Enqueue/
// RequeueOrphaned, serialized additionally by an in-process busy-repo set)
// -- this package does not replace or call into it. The two are
// deliberately parallel per loam-54o's "PARALLEL with the other stores"
// note; nothing in this tree wires this package to a consumer yet.
package ingestjobs

import (
	"github.com/bobcob7/loam/internal/db/gen"
)

//go:generate go tool moq -out moq_test.go . querier

// querier is the minimal Postgres surface Store needs: sqlc's generated
// DBTX. Defined here at the consumer, per repo convention, so Store can be
// unit-tested against a moq mock instead of a live pool; *pgxpool.Pool
// satisfies it in production without modification.
//
// Unlike internal/rolestore's querier, this package has no multi-statement
// write that needs a transaction: every write here (Enqueue, Claim,
// Complete, Fail, Requeue) is a single guarded statement -- Claim's own
// WITH ... UPDATE ... FROM candidate is one round trip whose row-level
// locking is scoped to that statement -- so there is no Begin method to
// expose and no inTx helper in this package.
type querier interface {
	gen.DBTX
}
