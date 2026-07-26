package reviewstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/db/gen"
)

// errDuplicateVerdict is returned when a submission hits
// verdicts_round_id_reviewer_key (docs/persistence-spec.md "verdicts")
// without going through the intended replace path -- a caller can then
// tell "this reviewer already voted in this round" apart from any other
// failure. Submit's own INSERT ... ON CONFLICT DO UPDATE means a normal
// re-submission never reaches this: it replaces the existing row in
// place. This mapping exists so that if the upsert clause were ever
// removed or bypassed, the resulting unique-violation still surfaces as
// this stable, distinguishable error rather than raw pgconn text.
var errDuplicateVerdict = errors.New("reviewer already voted in this round")

// Outcome is a verdict's decision, matching verdicts.outcome's CHECK
// constraint (docs/persistence-spec.md "verdicts").
type Outcome string

// The three outcomes verdicts.outcome's CHECK constraint allows.
const (
	OutcomeApprove    Outcome = "approve"
	OutcomeDisapprove Outcome = "disapprove"
	OutcomeNeutral    Outcome = "neutral"
)

// Verdict is one verdicts row.
type Verdict struct {
	ID        uuid.UUID
	RoundID   uuid.UUID
	Reviewer  string
	Outcome   Outcome
	CreatedAt time.Time
	UpdatedAt time.Time
}

// VerdictRecord is one verdict as returned by List, decorated with the
// round number it was cast in and whether that round is the work
// branch's CURRENT round. Current is computed by the same query that
// reads the row -- by comparing round numbers -- never read from a
// stored column, so a caller reading List cannot mistake a stale verdict
// for a current one by skipping a comparison step of its own.
type VerdictRecord struct {
	Verdict
	RoundNumber int32
	Current     bool
}

// VerdictStore implements verdicts: submitting (replacing on a repeat
// submission for the same round+reviewer), listing across rounds for
// history, and the current-round approve count the proposal queue and
// approval bar read.
type VerdictStore struct {
	q      *gen.Queries
	logger *slog.Logger
}

// NewVerdictStore builds a VerdictStore backed by db, typically a
// *pgxpool.Pool.
func NewVerdictStore(db querier, logger *slog.Logger) *VerdictStore {
	return &VerdictStore{q: gen.New(db), logger: logger}
}

// Submit records reviewer's outcome for roundID. Re-submitting for the
// same (roundID, reviewer) replaces the prior verdict in place --
// verdicts_round_id_reviewer_key (UNIQUE(round_id, reviewer)) is the
// constraint Demo M1 shows off: one verdict per reviewer per round.
func (s *VerdictStore) Submit(ctx context.Context, roundID uuid.UUID, reviewer string, outcome Outcome) (Verdict, error) {
	row, err := s.q.SubmitVerdict(ctx, gen.SubmitVerdictParams{
		ID:       pgUUID(uuid.New()),
		RoundID:  pgUUID(roundID),
		Reviewer: reviewer,
		Outcome:  string(outcome),
	})
	if err != nil {
		if isUniqueViolation(err, "verdicts_round_id_reviewer_key") {
			return Verdict{}, fmt.Errorf("submitting verdict for round %s reviewer %s: %w", roundID, reviewer, errDuplicateVerdict)
		}
		return Verdict{}, fmt.Errorf("submitting verdict for round %s reviewer %s: %w", roundID, reviewer, err)
	}
	s.logger.InfoContext(ctx, "submitted verdict", "round_id", roundID, "reviewer", reviewer, "outcome", outcome)
	return verdictFromRow(row), nil
}

// List returns every verdict across every round for workBranchID, newest
// round first, each decorated with whether it belongs to the branch's
// current round.
func (s *VerdictStore) List(ctx context.Context, workBranchID uuid.UUID) ([]VerdictRecord, error) {
	rows, err := s.q.ListVerdictsForWorkBranch(ctx, pgUUID(workBranchID))
	if err != nil {
		return nil, fmt.Errorf("listing verdicts for work branch %s: %w", workBranchID, err)
	}
	records := make([]VerdictRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, VerdictRecord{
			Verdict: Verdict{
				ID:        uuidFromPg(row.ID),
				RoundID:   uuidFromPg(row.RoundID),
				Reviewer:  row.Reviewer,
				Outcome:   Outcome(row.Outcome),
				CreatedAt: row.CreatedAt.Time,
				UpdatedAt: row.UpdatedAt.Time,
			},
			RoundNumber: row.RoundNumber,
			Current:     row.IsCurrentRound,
		})
	}
	return records, nil
}

// CurrentRoundApproveCount counts the approve-outcome verdicts cast in
// workBranchID's current round only -- backing the proposal queue and
// approval bar (docs/persistence-spec.md "verdicts"). A work branch with
// no rounds yet simply counts 0, not an error.
func (s *VerdictStore) CurrentRoundApproveCount(ctx context.Context, workBranchID uuid.UUID) (int64, error) {
	count, err := s.q.CurrentRoundApproveCount(ctx, pgUUID(workBranchID))
	if err != nil {
		return 0, fmt.Errorf("counting current-round approvals for work branch %s: %w", workBranchID, err)
	}
	return count, nil
}

// verdictFromRow converts a generated Verdict row to the package's own
// Verdict type, keeping pgtype/gen details out of this package's public
// surface.
func verdictFromRow(row gen.Verdict) Verdict {
	return Verdict{
		ID:        uuidFromPg(row.ID),
		RoundID:   uuidFromPg(row.RoundID),
		Reviewer:  row.Reviewer,
		Outcome:   Outcome(row.Outcome),
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
}
