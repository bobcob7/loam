//go:build integration

package workbranchstore

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedSequential creates n work branches in repoID via the real store,
// sleeping briefly between each so their DB-assigned created_at values are
// strictly increasing (the default sqlc/pgx timestamptz precision is
// microseconds, comfortably resolved by a millisecond sleep) -- this test
// needs a stable, known creation order to reason about which page each row
// belongs on, not just "some order".
func seedSequential(t *testing.T, store *Store, repoID uuid.UUID, n int) []WorkBranch {
	t.Helper()
	ctx := t.Context()
	rows := make([]WorkBranch, 0, n)
	for i := 0; i < n; i++ {
		wb, err := store.Create(ctx, repoID, fmt.Sprintf("wb-seed-%02d", i), "main", "grace-hopper-3-author")
		require.NoError(t, err)
		rows = append(rows, wb)
		time.Sleep(3 * time.Millisecond)
	}
	return rows
}

// TestListByCursor_ConcurrentInsert_NoRowSkippedOrDuplicated is loam-coj's
// concurrency proof. It pages through a real Postgres table with a small
// page size, creating new rows BETWEEN page fetches -- a real INSERT
// committed against the live database while the scan is still in
// progress, interleaved deterministically at known points rather than
// raced from a goroutine (which would only hit the exact interleaving
// probabilistically) -- and asserts that every row present at any point
// during the scan, whether seeded before it started or created while it
// ran, is returned EXACTLY once: never missing, never duplicated.
//
// # Why this table's hazard is a missing INSERT, not a missing pre-existing row
//
// work_branches rows are never deleted or have created_at rewritten (no
// query in work_branches.sql does either), so every row's position in
// created_at order is fixed once written. Empirically probing the OLD
// LIMIT/OFFSET implementation (Store.List) against a real Postgres with a
// row inserted between two page fetches showed it NEVER loses a
// pre-existing row -- OFFSET's "rows already consumed" count and the
// re-fetched total stay internally consistent across the whole pass for an
// insert-only table -- but it DOES lose exactly the newly-inserted row,
// silently: the concurrent INSERT sorts into the region the scan has
// already moved past (DESC = newest first, and a fresh row is always the
// newest), so it is masked by an off-by-one duplicate of whatever
// pre-existing row lands on the shifted boundary and never appears in any
// page. For StoreRepoResolver (internal/mirrorsync), that missing row is a
// work branch whose ref was just created and is now silently absent from
// THIS pass's mirror-fetch exclusion list -- exactly the unrecoverable-
// deletion hazard loam-coj exists to close.
//
// Switching Store.List's OFFSET to a same-direction (DESC) keyset compare
// does NOT close that gap by itself: it removes the duplicate (proven by a
// second probe), but a row that is the newest thing in the table when it
// commits still sorts behind wherever a DESC-ordered cursor has already
// advanced to, so it is just as invisible to a keyset scan moving newest
// -> oldest as it is to OFFSET. ListByCursor is therefore ordered
// OLDEST-first (ASC), the reverse of List -- see
// ListWorkBranchesByCursor's doc comment (internal/db/queries/
// work_branches.sql) for the full reasoning. Under ASC order a
// concurrently-created row, always the newest row in the table at the
// instant it commits, sorts to the very END of the scan: into whatever is
// still ahead of the cursor, never behind it, so a still-in-progress
// enumeration reaches it exactly like any other not-yet-visited row. A
// third probe against ASC-ordered keyset confirmed this captures the
// concurrent insert; this test pins that result as a permanent regression
// check.
//
// # Why this test fails against the pre-loam-coj implementation
//
// Re-pointing this scenario's page-fetch loop at Store.List (LIMIT/OFFSET,
// still present and unchanged -- it remains the wire API's PageInfo
// primitive) reproduces the probe's finding directly: the row created
// after page 1 is never returned by any subsequent page, so the assertion
// below on rows created DURING the scan fails. That re-pointed run is not
// kept in this file (a permanently-failing test would break `task
// test:integration`); it was run manually before this fix landed and its
// output is recorded in loam-coj's closing notes.
func TestListByCursor_ConcurrentInsert_NoRowSkippedOrDuplicated(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	const pageSize = 2
	seeded := seedSequential(t, store, repoID, 4)
	var pages [][]WorkBranch
	var concurrentlyInserted []WorkBranch
	var after *Cursor
	pageNum := 0
	for {
		page, err := store.ListByCursor(ctx, ListFilter{RepoID: &repoID}, pageSize, after)
		require.NoError(t, err)
		pageNum++
		if len(page) == 0 {
			break
		}
		pages = append(pages, page)
		if pageNum == 1 || pageNum == 2 {
			wb, err := store.Create(ctx, repoID, fmt.Sprintf("wb-concurrent-%02d", pageNum), "main", "alan-turing-4-author")
			require.NoError(t, err, "the concurrent insert simulating another goroutine's Create mid-scan")
			concurrentlyInserted = append(concurrentlyInserted, wb)
		}
		last := page[len(page)-1]
		after = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	require.Len(t, concurrentlyInserted, 2, "precondition: both concurrent inserts must actually have happened during the scan, not after it ended")
	seen := make(map[uuid.UUID]int)
	for _, page := range pages {
		for _, wb := range page {
			seen[wb.ID]++
		}
	}
	for _, wb := range seeded {
		assert.Equal(t, 1, seen[wb.ID], "seeded row %s must be returned exactly once", wb.Name)
	}
	for _, wb := range concurrentlyInserted {
		assert.Equal(t, 1, seen[wb.ID], "row %s, created WHILE the scan was in progress, must still be captured -- a silently missing row here is the unrecoverable-deletion hazard for StoreRepoResolver's exclusion list", wb.Name)
	}
}
