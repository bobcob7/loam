//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon. Run with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/reviewstore/... -v
//
// (see internal/db/migrations/integration_test.go for why
// TESTCONTAINERS_RYUK_DISABLED is a podman-only workaround, not a CI
// setting).
//
// These tests apply the REAL migration set, so threads' and comments'
// nullable file/line anchors, their round_id foreign keys, and
// ResolveThread's author guard are the actual constraints Postgres
// enforces -- not a hand-rolled test schema that could drift from what
// ships.
//
// DEFERRED-WIP: replies.feature: "A reply records the round it was made in"
// -> TestReply_LandsInALaterRoundThanItsThread covers the STORE half in
// full (the reply carries round 2 while its thread still reads round 1).
// The godog scenario stays @wip until the CLI half (loam-0pj.13) exists.
//
// DEFERRED-WIP: reviewing.feature: "Only the thread's author may resolve
// it" -> TestResolve_OnlyTheAuthorMayResolve covers the store half; the
// handler half is internal/handler/workbranch's unit tests and
// internal/reviewpublish's rollback test.
package reviewstore

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openRound is a shorthand for the round every thread and comment must be
// stamped against -- threads.round_id and comments.round_id are both NOT
// NULL foreign keys, so there is no such thing as a round-less thread.
func openRound(ctx context.Context, t *testing.T, pool *pgxpool.Pool, workBranchID uuid.UUID) Round {
	t.Helper()
	round, err := NewRoundStore(pool, testLogger()).OpenRound(ctx, workBranchID, "grace-hopper-3-author")
	require.NoError(t, err)
	return round
}

// TestOpenThread_AnchoredAndUnanchored proves both thread shapes round-trip
// through the real nullable columns: an anchored thread keeps its file and
// line, and a top-level thread stores SQL NULL in both -- read back as nil,
// never as "" or 0, which a client would render as a comment on a file
// named "" at line 0.
func TestOpenThread_AnchoredAndUnanchored(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	threads := NewThreadStore(pool, testLogger())
	workBranchID := newWorkBranch(ctx, t, pool, "anchors-repo")
	round := openRound(ctx, t, pool, workBranchID)
	file, line := "auth.go", int32(42)
	anchored, err := threads.OpenThread(ctx, workBranchID, round.ID, round.Number, "ada-lovelace-7-reviewer", &file, &line, "needs a guard")
	require.NoError(t, err)
	require.NotNil(t, anchored.File)
	assert.Equal(t, "auth.go", *anchored.File)
	require.NotNil(t, anchored.Line)
	assert.Equal(t, int32(42), *anchored.Line)
	require.Len(t, anchored.Comments, 1, "the opening comment is created by the same statement as the thread")
	assert.Equal(t, "needs a guard", anchored.Comments[0].Body)

	topLevel, err := threads.OpenThread(ctx, workBranchID, round.ID, round.Number, "ada-lovelace-7-reviewer", nil, nil, "overall this reads well")
	require.NoError(t, err)
	assert.Nil(t, topLevel.File, "a top-level thread stores SQL NULL, not an empty path")
	assert.Nil(t, topLevel.Line)

	listed, total, err := threads.List(ctx, workBranchID, 100, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	require.Len(t, listed, 2)
	assert.Nil(t, listed[1].File, "the anchor's nullability survives the read path too, not just the write")
}

// TestOpenThread_NeverProducesACommentlessThread proves the CTE is doing
// the work the comment on OpenThreadWithComment claims: the thread and its
// opening comment are one statement, so there is no window in which the
// thread exists alone. Checked by reading the raw tables -- a thread row
// with zero comment rows is exactly the state the single statement makes
// unreachable.
func TestOpenThread_NeverProducesACommentlessThread(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	threads := NewThreadStore(pool, testLogger())
	workBranchID := newWorkBranch(ctx, t, pool, "atomic-thread-repo")
	round := openRound(ctx, t, pool, workBranchID)
	opened, err := threads.OpenThread(ctx, workBranchID, round.ID, round.Number, "ada-lovelace-7-reviewer", nil, nil, "first")
	require.NoError(t, err)
	var comments int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM comments WHERE thread_id = $1`, opened.ID).Scan(&comments))
	assert.Equal(t, 1, comments)
	var commentRound uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT round_id FROM comments WHERE thread_id = $1`, opened.ID).Scan(&commentRound))
	assert.Equal(t, round.ID, commentRound, "the opening comment is stamped with the SAME round as its thread")
}

// TestReply_LandsInALaterRoundThanItsThread is replies.feature's "A reply
// records the round it was made in" at the store: a thread raised in round
// 1 collects a reply after round 2 opens, and the two round numbers must
// come back DIFFERENT -- the reply in round 2, the thread still reading
// round 1. A store that derived a comment's round from its thread (or a
// thread's round from its newest comment) fails here.
func TestReply_LandsInALaterRoundThanItsThread(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	threads := NewThreadStore(pool, testLogger())
	workBranchID := newWorkBranch(ctx, t, pool, "later-round-repo")
	round1 := openRound(ctx, t, pool, workBranchID)
	opened, err := threads.OpenThread(ctx, workBranchID, round1.ID, round1.Number, "ada-lovelace-7-reviewer", nil, nil, "needs a guard")
	require.NoError(t, err)
	round2 := openRound(ctx, t, pool, workBranchID)
	require.Equal(t, int32(2), round2.Number)
	reply, err := threads.Reply(ctx, opened.ID, round2.ID, round2.Number, "grace-hopper-3-author", "thanks, fixed")
	require.NoError(t, err)
	assert.Equal(t, int32(2), reply.RoundNumber)

	listed, _, err := threads.List(ctx, workBranchID, 100, 0)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, int32(1), listed[0].RoundNumber, "the thread still shows it was raised in the first round")
	require.Len(t, listed[0].Comments, 2)
	assert.Equal(t, int32(1), listed[0].Comments[0].RoundNumber, "the opening comment stays in round 1")
	assert.Equal(t, int32(2), listed[0].Comments[1].RoundNumber, "the reply records the round it was made in")
	assert.Equal(t, "grace-hopper-3-author", listed[0].Comments[1].Author)
}

// TestResolve_OnlyTheAuthorMayResolve is reviewing.feature's "Only the
// thread's author may resolve it" against the real guarded UPDATE: the
// author's resolve lands, another agent's is rejected with
// ErrNotThreadAuthor, and -- the assertion that matters -- the rejected
// attempt leaves resolved FALSE in the database, so the guard is genuinely
// in the WHERE clause and not merely reported after the fact.
func TestResolve_OnlyTheAuthorMayResolve(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	threads := NewThreadStore(pool, testLogger())
	workBranchID := newWorkBranch(ctx, t, pool, "resolve-repo")
	round := openRound(ctx, t, pool, workBranchID)
	mine, err := threads.OpenThread(ctx, workBranchID, round.ID, round.Number, "ada-lovelace-7-reviewer", nil, nil, "mine")
	require.NoError(t, err)
	theirs, err := threads.OpenThread(ctx, workBranchID, round.ID, round.Number, "alan-turing-4-reviewer", nil, nil, "theirs")
	require.NoError(t, err)

	resolved, err := threads.Resolve(ctx, workBranchID, mine.ID, "ada-lovelace-7-reviewer")
	require.NoError(t, err)
	assert.True(t, resolved.Resolved)

	_, err = threads.Resolve(ctx, workBranchID, theirs.ID, "ada-lovelace-7-reviewer")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotThreadAuthor)
	var stillOpen bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT resolved FROM threads WHERE id = $1`, theirs.ID).Scan(&stillOpen))
	assert.False(t, stillOpen, "a rejected resolve must not have written anything")
}

// TestResolve_UnknownOrForeignThread_ReturnsErrThreadNotFound proves both
// misses report the SAME error: a thread id that does not exist, and one
// that exists on a DIFFERENT work branch. Reporting the second any
// differently would let a caller probe another work branch's thread ids for
// existence.
func TestResolve_UnknownOrForeignThread_ReturnsErrThreadNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	threads := NewThreadStore(pool, testLogger())
	mine := newWorkBranch(ctx, t, pool, "scoped-a-repo")
	theirs := newWorkBranch(ctx, t, pool, "scoped-b-repo")
	theirRound := openRound(ctx, t, pool, theirs)
	foreign, err := threads.OpenThread(ctx, theirs, theirRound.ID, theirRound.Number, "ada-lovelace-7-reviewer", nil, nil, "elsewhere")
	require.NoError(t, err)

	_, err = threads.Resolve(ctx, mine, uuid.New(), "ada-lovelace-7-reviewer")
	assert.ErrorIs(t, err, ErrThreadNotFound)
	_, err = threads.Resolve(ctx, mine, foreign.ID, "ada-lovelace-7-reviewer")
	assert.ErrorIs(t, err, ErrThreadNotFound, "a thread on another work branch is indistinguishable from one that does not exist")
	_, err = threads.Get(ctx, mine, foreign.ID)
	assert.ErrorIs(t, err, ErrThreadNotFound)
}

// TestList_PaginatesByThread proves the page unit is the thread, not the
// comment: three threads with differing comment counts paginate 2 + 1, each
// page carrying its threads' full comment sets and the same total.
func TestList_PaginatesByThread(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	threads := NewThreadStore(pool, testLogger())
	workBranchID := newWorkBranch(ctx, t, pool, "paging-repo")
	round := openRound(ctx, t, pool, workBranchID)
	first, err := threads.OpenThread(ctx, workBranchID, round.ID, round.Number, "ada-lovelace-7-reviewer", nil, nil, "one")
	require.NoError(t, err)
	_, err = threads.Reply(ctx, first.ID, round.ID, round.Number, "grace-hopper-3-author", "reply to one")
	require.NoError(t, err)
	_, err = threads.OpenThread(ctx, workBranchID, round.ID, round.Number, "ada-lovelace-7-reviewer", nil, nil, "two")
	require.NoError(t, err)
	_, err = threads.OpenThread(ctx, workBranchID, round.ID, round.Number, "ada-lovelace-7-reviewer", nil, nil, "three")
	require.NoError(t, err)

	page1, total, err := threads.List(ctx, workBranchID, 2, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total, "total counts threads, not comments")
	require.Len(t, page1, 2)
	assert.Len(t, page1[0].Comments, 2, "a thread's comments are never split across pages")
	page2, total, err := threads.List(ctx, workBranchID, 2, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	require.Len(t, page2, 1)
	assert.Equal(t, "three", page2[0].Comments[0].Body)
}

// TestList_NoThreads_IsEmptyNotAnError proves a work branch nobody has
// commented on lists cleanly -- the ANY($1::uuid[]) comment fetch is
// skipped entirely rather than being handed an empty array.
func TestList_NoThreads_IsEmptyNotAnError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	threads := NewThreadStore(pool, testLogger())
	workBranchID := newWorkBranch(ctx, t, pool, "silent-repo")
	listed, total, err := threads.List(ctx, workBranchID, 100, 0)
	require.NoError(t, err)
	assert.Empty(t, listed)
	assert.Equal(t, int64(0), total)
}
