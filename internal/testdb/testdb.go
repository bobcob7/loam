// Package testdb holds constants shared by integration tests that spin up a
// real Postgres via testcontainers-go. It has no runtime dependents outside
// tests -- the naming mirrors internal/testembed and internal/testfixture,
// the repo's existing "test<noun>" test-support packages, rather than the
// dbtest ordering the bead that introduced this package originally floated,
// so it reads consistently alongside its siblings.
package testdb

// PostgresImage is the Postgres image every integration test that runs
// migrations.Migrate must use: migration 0002_code_intel issues CREATE
// EXTENSION vector, and plain postgres:16-alpine has no vector extension
// available to create at all. pgvector/pgvector:pg16 ships the extension
// built in. Centralized here (loam-eh0) so the four store beads and
// loam-li0.6 landing this wave add call sites, not new copies of the
// literal.
const PostgresImage = "pgvector/pgvector:pg16"
