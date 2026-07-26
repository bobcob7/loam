package reviewstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bobcob7/loam/internal/db/gen"
)

// errNoCurrentRound is returned by CurrentRound when a work branch has no
// review_rounds row yet (review has never been requested for it) --
// distinguishable from a transport failure so a caller can tell "no round
// yet" apart from "the database is unreachable".
var errNoCurrentRound = errors.New("work branch has no current review round")

// errRoundNumberConflict is returned when two concurrent OpenRound calls
// for the same work branch race: review_rounds_work_branch_id_number_key
// (docs/persistence-spec.md "review_rounds") rejects the loser rather than
// silently double-assigning a round number. OpenRound computes the next
// number as MAX(number)+1 in the same statement as the insert, so this is
// a genuine constraint hit, not a bug -- the caller may simply retry.
var errRoundNumberConflict = errors.New("review round number conflict")

// Round is one review_rounds row.
type Round struct {
	ID           uuid.UUID
	WorkBranchID uuid.UUID
	Number       int32
	RequestedBy  string
	CreatedAt    time.Time
}

// RoundStore implements review_rounds: opening a new round on every
// transition into reviewable, and exposing the current round -- the row
// with the highest number -- that VerdictStore's staleness comparisons
// and CurrentRoundApproveCount are computed against.
type RoundStore struct {
	q      *gen.Queries
	logger *slog.Logger
}

// NewRoundStore builds a RoundStore backed by db, typically a
// *pgxpool.Pool.
func NewRoundStore(db querier, logger *slog.Logger) *RoundStore {
	return &RoundStore{q: gen.New(db), logger: logger}
}

// OpenRound opens a new review round for workBranchID: number =
// MAX(number)+1 for that branch (1 if none exist yet). Called on every
// transition into reviewable -- author request-review, admin send-back,
// and the catch-up auto-restore (docs/git-spec.md).
func (s *RoundStore) OpenRound(ctx context.Context, workBranchID uuid.UUID, requestedBy string) (Round, error) {
	row, err := s.q.OpenReviewRound(ctx, gen.OpenReviewRoundParams{
		ID:           pgUUID(uuid.New()),
		WorkBranchID: pgUUID(workBranchID),
		RequestedBy:  requestedBy,
	})
	if err != nil {
		if isUniqueViolation(err, "review_rounds_work_branch_id_number_key") {
			return Round{}, fmt.Errorf("opening round for work branch %s: %w", workBranchID, errRoundNumberConflict)
		}
		return Round{}, fmt.Errorf("opening round for work branch %s: %w", workBranchID, err)
	}
	s.logger.InfoContext(ctx, "opened review round", "work_branch_id", workBranchID, "number", row.Number, "requested_by", requestedBy)
	return roundFromRow(row), nil
}

// CurrentRound returns workBranchID's current round: the review_rounds row
// with the highest number. This is the single source every "is this
// stale" comparison in this package derives from -- never a stored flag.
func (s *RoundStore) CurrentRound(ctx context.Context, workBranchID uuid.UUID) (Round, error) {
	row, err := s.q.CurrentReviewRound(ctx, pgUUID(workBranchID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Round{}, fmt.Errorf("getting current round for work branch %s: %w", workBranchID, errNoCurrentRound)
		}
		return Round{}, fmt.Errorf("getting current round for work branch %s: %w", workBranchID, err)
	}
	return roundFromRow(row), nil
}

// roundFromRow converts a generated ReviewRound row to the package's own
// Round type, keeping pgtype/gen details out of this package's public
// surface.
func roundFromRow(row gen.ReviewRound) Round {
	return Round{
		ID:           uuidFromPg(row.ID),
		WorkBranchID: uuidFromPg(row.WorkBranchID),
		Number:       row.Number,
		RequestedBy:  row.RequestedBy,
		CreatedAt:    row.CreatedAt.Time,
	}
}
