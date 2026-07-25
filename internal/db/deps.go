package db

// The blank imports below pin github.com/google/uuid and
// github.com/pgvector/pgvector-go as direct module requires ahead of their
// first real use in this codebase. UUIDv7 primary keys (uuid.NewV7) and the
// pgvector Vector column type are conventions this bead's governing spec
// (docs/persistence-spec.md) commits the whole persistence layer to, but the
// store and sqlc beads that will actually call them cannot edit go.mod
// themselves — this bead's file territory is internal/db/** plus
// go.mod/go.sum only, per loam-54o.1's acceptance criteria. Pinning the
// import here (rather than leaving a bare go.mod edit) means `go mod tidy`
// keeps these direct instead of demoting them back to indirect.
import (
	_ "github.com/google/uuid"
	_ "github.com/pgvector/pgvector-go"
)
