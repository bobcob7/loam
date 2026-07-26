package workbranchstore

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// pgUUID adapts a uuid.UUID to the pgtype.UUID sqlc's generated params
// expect; uuid.UUID and pgtype.UUID's Bytes are both a plain [16]byte in
// the same byte order, so this is a direct field copy, not a reparse.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// uuidFromPg adapts a pgtype.UUID scanned off a generated row back to a
// uuid.UUID. Every id/foreign-key column work_branches carries is NOT NULL
// (docs/persistence-spec.md "work_branches"), so a row that scanned
// successfully always carries Valid: true here.
func uuidFromPg(id pgtype.UUID) uuid.UUID {
	return id.Bytes
}

// pgText adapts a plain string to a always-valid pgtype.Text -- used for
// SetTitleDescription's title/description, which this package treats as a
// full replace (never a SQL NULL write); see Store.SetTitleDescription's
// doc comment for why.
func pgText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
}

// textFromPg adapts a nullable pgtype.Text scanned off a generated row to
// a *string: nil when the column is SQL NULL (title, description,
// upstream_pr_url, and close_reason are all nullable,
// docs/persistence-spec.md "work_branches"), a non-nil pointer to the
// value otherwise.
func textFromPg(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

// int4FromPg adapts a nullable pgtype.Int4 (upstream_pr_number) to a
// *int32: nil when the column is SQL NULL, a non-nil pointer otherwise.
func int4FromPg(i pgtype.Int4) *int32 {
	if !i.Valid {
		return nil
	}
	return &i.Int32
}
