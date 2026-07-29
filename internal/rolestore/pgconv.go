package rolestore

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// uuidFromPG converts a valid pgtype.UUID (as returned by a successful
// query) back to uuid.UUID. roles.id is NOT NULL, so a row that scanned
// successfully always carries Valid: true here.
func uuidFromPG(id pgtype.UUID) uuid.UUID {
	return uuid.UUID(id.Bytes)
}

// pgUUID converts a generated uuid.UUID to the pgtype.UUID sqlc's params
// structs take. Valid is always true: this package only ever passes ids it
// generated itself (uuid.NewV7) or read back out of a row.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
