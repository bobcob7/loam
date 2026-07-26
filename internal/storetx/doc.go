// Package storetx holds the integration test that proves loam-2ph's
// acceptance criterion end to end: a caller that binds internal/codegraph,
// internal/chunkstore, and internal/reposstore to the SAME pgx.Tx (via each
// package's NewInTx/NewStoreInTx constructor) and writes through all three
// inside it produces exactly one commit, and a concurrent reader on a
// separate connection sees the prior state until that commit lands and the
// new state only after -- the Postgres MVCC snapshot property
// loam-c94.12's one-transaction atomic swap depends on
// (docs/ingestion-spec.md "Consistency & Failure": "readers see the prior
// index until it commits").
//
// This package holds no production code of its own -- every type and
// constructor it exercises belongs to codegraph, chunkstore, or reposstore.
// It exists only because the property under test spans those three
// packages and belongs to none of them individually.
package storetx
