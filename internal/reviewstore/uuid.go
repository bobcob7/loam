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

// pgTextPtr adapts an optional string (a thread's file anchor, NULL for a
// top-level thread) to the nullable pgtype.Text the generated params
// expect: nil becomes SQL NULL, never the empty string.
func pgTextPtr(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

// pgInt4Ptr adapts an optional int32 (a thread's line anchor) to the
// nullable pgtype.Int4 the generated params expect: nil becomes SQL NULL,
// never 0.
func pgInt4Ptr(i *int32) pgtype.Int4 {
	if i == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *i, Valid: true}
}

// textFromPg adapts a nullable pgtype.Text scanned off a generated row
// (threads.file) to a *string: nil when the column is SQL NULL, a non-nil
// pointer otherwise.
func textFromPg(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

// int4FromPg adapts a nullable pgtype.Int4 scanned off a generated row
// (threads.line) to a *int32: nil when the column is SQL NULL, a non-nil
// pointer otherwise.
func int4FromPg(i pgtype.Int4) *int32 {
	if !i.Valid {
		return nil
	}
	return &i.Int32
}
