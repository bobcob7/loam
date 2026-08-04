package ingestjobs

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bobcob7/loam/internal/db/gen"
)

// pgUUID adapts a uuid.UUID to the pgtype.UUID sqlc's generated params
// expect; uuid.UUID and pgtype.UUID's Bytes are both a plain [16]byte in
// the same byte order, so this is a direct field copy, not a reparse.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// uuidFromPg adapts a pgtype.UUID scanned off a generated row back to a
// uuid.UUID. ingest_jobs.id and ingest_jobs.repo_id are both NOT NULL
// (docs/persistence-spec.md "ingest_jobs"), so a row that scanned
// successfully always carries Valid: true here.
func uuidFromPg(id pgtype.UUID) uuid.UUID {
	return id.Bytes
}

// pgText adapts a nullable *string to pgtype.Text, NULL (Valid: false)
// when s is nil -- used for FailIngestJobParams.Error.
func pgText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

// textFromPg adapts a nullable pgtype.Text scanned off a generated row to
// a *string: nil when the column is SQL NULL (ingest_jobs.error is
// nullable, docs/persistence-spec.md "ingest_jobs"), a non-nil pointer to
// the value otherwise.
func textFromPg(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

// timeFromPg adapts a nullable pgtype.Timestamptz scanned off a generated
// row to a *time.Time: nil when the column is SQL NULL (started_at and
// finished_at both start NULL, docs/persistence-spec.md "ingest_jobs"), a
// non-nil pointer otherwise.
func timeFromPg(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// jsonFromBytes adapts jsonb bytes sqlc scans as []byte (stats is
// nullable, so a NULL column scans as a nil slice) to json.RawMessage:
// nil stays nil rather than becoming an empty-but-non-nil RawMessage, so a
// caller checking "has this job recorded stats yet" can do so with a
// plain nil check.
func jsonFromBytes(b []byte) json.RawMessage {
	if b == nil {
		return nil
	}
	return json.RawMessage(b)
}

// fromGenIngestJob converts a sqlc-generated row into the store's own Job
// type, keeping pgtype/gen details out of this package's public surface.
func fromGenIngestJob(row gen.IngestJob) Job {
	return Job{
		ID:           uuidFromPg(row.ID),
		RepoID:       uuidFromPg(row.RepoID),
		TargetBranch: row.TargetBranch,
		Kind:         Kind(row.Kind),
		Status:       Status(row.Status),
		Attempts:     row.Attempts,
		Error:        textFromPg(row.Error),
		Stats:        jsonFromBytes(row.Stats),
		QueuedAt:     row.QueuedAt.Time,
		StartedAt:    timeFromPg(row.StartedAt),
		FinishedAt:   timeFromPg(row.FinishedAt),
	}
}
