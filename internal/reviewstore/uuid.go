package reviewstore

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// pgUUID adapts a uuid.UUID to the pgtype.UUID sqlc's generated params
// expect.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// uuidFromPg adapts a pgtype.UUID scanned off a generated row back to a
// uuid.UUID. Every id/foreign-key column in this table group is NOT NULL
// (docs/persistence-spec.md "review_rounds", "verdicts"), so a row that
// scanned successfully always carries Valid: true here.
func uuidFromPg(id pgtype.UUID) uuid.UUID {
	return id.Bytes
}
