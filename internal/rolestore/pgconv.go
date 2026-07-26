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
