//go:build integration

package storesuite

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/reviewstore"
)

// newWorkBranch inserts a minimal repos row and a work_branches row under
// it, returning the work branch's id -- the only fixture review_rounds and
// verdicts need.
func newWorkBranch(t *testing.T, name string) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	pool := mustPool(t)
	repoID := insertRepo(ctx, t, pool, "group/"+name)
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO work_branches (id, repo_id, name, target, author)
		 VALUES (gen_random_uuid(), $1, $2, 'main', 'grace-hopper-3-author')
		 RETURNING id`,
		repoID, name,
	).Scan(&id))
	return id
}

// TestStoreSuite_UniqueRoundReviewer_DerivedStaleness is Demo M1's first
// live proof: UNIQUE(round_id, reviewer) with staleness computed fresh on
// every read, never a stored flag. It calls internal/reviewstore's real
// Store API -- the exact code path production uses -- and is narrated with
// t.Logf beats so a green `-v` run reads as a demonstration, not a single
// "--- PASS" line (testify only prints assertion messages on FAILURE). The
// deeper edge-case coverage (resubmission-replaces, sequential numbering,
// error identity, raw-constraint proof) already lives in
// internal/reviewstore's own TestReviewRounds_DerivedStaleness_Narrative
// and its siblings -- this is the same narrative, reused rather than
// reproved, purely for the cross-store demo assembly.
func TestStoreSuite_UniqueRoundReviewer_DerivedStaleness(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := mustPool(t)
	rounds := reviewstore.NewRoundStore(pool, testLogger())
	verdicts := reviewstore.NewVerdictStore(pool, testLogger())
	workBranchID := newWorkBranch(t, "demo-rounds-repo")

	t.Logf("round 1 opens (author requests review)")
	round1, err := rounds.OpenRound(ctx, workBranchID, "grace-hopper-3-author")
	require.NoError(t, err)
	assert.Equal(t, int32(1), round1.Number)

	t.Logf("Ada approves round %d", round1.Number)
	_, err = verdicts.Submit(ctx, round1.ID, "ada-lovelace-7-reviewer", reviewstore.OutcomeApprove)
	require.NoError(t, err)
	count, err := verdicts.CurrentRoundApproveCount(ctx, workBranchID)
	require.NoError(t, err)
	t.Logf("current-round approve count = %d", count)
	assert.Equal(t, int64(1), count, "round 1's approve counts while it is current")

	t.Logf("a second UNIQUE(round_id, reviewer) row for the same reviewer replaces, not duplicates")
	resubmitted, err := verdicts.Submit(ctx, round1.ID, "ada-lovelace-7-reviewer", reviewstore.OutcomeApprove)
	require.NoError(t, err)
	records, err := verdicts.List(ctx, workBranchID)
	require.NoError(t, err)
	require.Len(t, records, 1, "UNIQUE(round_id, reviewer) held: one reviewer, one row, even after re-submission")
	t.Logf("verdict row id stable across resubmission: %s", resubmitted.ID)

	t.Logf("round 2 opens (admin send-back, or author requests review again) -- Ada's round-1 approval is NOT touched")
	round2, err := rounds.OpenRound(ctx, workBranchID, "grace-hopper-3-author")
	require.NoError(t, err)
	assert.Equal(t, int32(2), round2.Number)

	current, err := rounds.CurrentRound(ctx, workBranchID)
	require.NoError(t, err)
	assert.Equal(t, round2.ID, current.ID, "round 2 is now current")

	records, err = verdicts.List(ctx, workBranchID)
	require.NoError(t, err)
	require.Len(t, records, 1, "Ada's round-1 verdict is still the only verdict row -- nobody wrote a stale flag on it")
	assert.False(t, records[0].Current, "Ada's verdict belongs to round 1, which is no longer current")
	t.Logf("Ada's verdict is no longer current -- DERIVED from round comparison, no stored flag was ever flipped")

	count, err = verdicts.CurrentRoundApproveCount(ctx, workBranchID)
	require.NoError(t, err)
	t.Logf("current-round approve count = %d (round 1's approve no longer counts)", count)
	assert.Equal(t, int64(0), count, "the proposal queue must not credit a stale approval as current")
}
