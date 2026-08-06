package diffplan

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/bobcob7/loam/internal/ingest"
)

// These are loam-qj21's tests for the retry union. They are pure -- no git,
// no mirror -- because WithRetryPaths is pure: it is the one place a path
// that `git diff old..new` structurally CANNOT report gets back into a
// plan, and everything it decides is decidable from the Plan and the path
// list alone.

// TestWithRetryPaths_AddsALedgeredPathThatTheDiffCannotName is the defect
// in one assertion. The plan is what an incremental ingest looks like when
// nothing the ledger cares about changed -- one unrelated file modified --
// and the ledgered path is absent from it, which is exactly the state that
// made a rejected file unreachable forever: it did not change between
// ingested_ref and tip, so no diff can name it, so it was never reparsed.
func TestWithRetryPaths_AddsALedgeredPathThatTheDiffCannotName(t *testing.T) {
	t.Parallel()
	plan := Plan{Kind: ingest.KindIncremental, ReparseFiles: []string{"unrelated.go"}}

	got := plan.WithRetryPaths([]string{"rejected.go"})

	assert.Equal(t, []string{"unrelated.go", "rejected.go"}, got.ReparseFiles,
		"the ledgered path must join the reparse set even though the diff never mentioned it -- that is the whole retry")
	assert.Equal(t, ingest.KindIncremental, got.Kind, "a retry adds files to an incremental plan; it is not an escalation")
	assert.Empty(t, got.Reason, "adding retry paths must not fabricate an escalation reason")
}

// TestWithRetryPaths_LeavesAFullPlanCompletelyAlone pins the rule with the
// sharpest failure mode. A full plan's ReparseFiles is already every file
// in the tree at NewRef, so a ledgered path still present is in it. A
// ledgered path NOT in it is one the tree no longer contains at all --
// after a force-push, a history rewrite, or simply a deletion the escalated
// plan never diffed -- and appending it would ask the mirror for a blob
// that is not at NewRef.
//
// The fixture makes that distinguishable on purpose: "gone.go" is in
// neither the tree nor the drop list, so a naive union would append it and
// this assertion would catch it. Asserting on the exact slice rather than
// its length is what makes that so.
func TestWithRetryPaths_LeavesAFullPlanCompletelyAlone(t *testing.T) {
	t.Parallel()
	plan := Plan{Kind: ingest.KindFull, Reason: "first ingest", ReparseFiles: []string{"a.go", "b.go"}}

	got := plan.WithRetryPaths([]string{"gone.go", "a.go"})

	assert.Equal(t, []string{"a.go", "b.go"}, got.ReparseFiles,
		"a full plan already reparses the whole tree; a ledgered path missing from it is a path the tree no longer has")
	assert.Equal(t, plan, got, "nothing about a full plan may change")
}

// TestWithRetryPaths_SkipsAPathThePlanIsDropping covers the file that was
// rejected and then DELETED. It is in DropFiles, so it does not resolve to
// a blob at NewRef; reparsing it would fail the read (or, worse, silently
// read nothing and look like a success). Nothing is owed for a file that no
// longer exists, and the caller clears its ledger row in the same
// transaction.
func TestWithRetryPaths_SkipsAPathThePlanIsDropping(t *testing.T) {
	t.Parallel()
	plan := Plan{
		Kind:         ingest.KindIncremental,
		DropFiles:    []string{"deleted.go"},
		ReparseFiles: []string{"kept.go"},
	}

	got := plan.WithRetryPaths([]string{"deleted.go", "still-owed.go"})

	assert.Equal(t, []string{"kept.go", "still-owed.go"}, got.ReparseFiles,
		"a dropped path must not be reparsed; every other ledgered path still must be")
	assert.Equal(t, []string{"deleted.go"}, got.DropFiles, "the drop list itself is untouched")
}

// TestWithRetryPaths_DoesNotDuplicateAPathTheDiffAlreadyNames is the
// ordinary case once a rejection is fixed: someone edits the broken file,
// so it is in the diff on its own merits AND in the ledger. Reparsing it
// twice would re-embed it twice (a real network cost) and would write its
// chunks twice in one transaction.
func TestWithRetryPaths_DoesNotDuplicateAPathTheDiffAlreadyNames(t *testing.T) {
	t.Parallel()
	plan := Plan{Kind: ingest.KindIncremental, ReparseFiles: []string{"fixed.go", "other.go"}}

	got := plan.WithRetryPaths([]string{"fixed.go", "fixed.go", "new.go"})

	assert.Equal(t, []string{"fixed.go", "other.go", "new.go"}, got.ReparseFiles,
		"deduped against the plan AND against the retry list itself")
}

// TestWithRetryPaths_EmptyLedgerReturnsTheSamePlan is the healthy case,
// which is every ingest of every repo that has never had a rejection. It
// must be exactly a no-op -- not "equivalent", the same value -- so that
// nothing downstream can behave differently on a repo that merely has the
// ledger wired up.
func TestWithRetryPaths_EmptyLedgerReturnsTheSamePlan(t *testing.T) {
	t.Parallel()
	plan := Plan{Kind: ingest.KindIncremental, DropFiles: []string{"d.go"}, ReparseFiles: []string{"r.go"}}

	assert.Equal(t, plan, plan.WithRetryPaths(nil))
	assert.Equal(t, plan, plan.WithRetryPaths([]string{}))
	assert.Equal(t, plan, plan.WithRetryPaths([]string{"r.go"}),
		"a ledgered path the diff already names adds nothing, so the plan is unchanged")
}

// TestWithRetryPaths_DoesNotAliasTheReceiversSlice guards a bug that would
// be invisible in every other test here: appending into p.ReparseFiles'
// own backing array would let the returned plan write through the one the
// caller still holds. Plan is passed by value, which makes it look immune,
// and it is not -- the slice header is copied, the array is not.
func TestWithRetryPaths_DoesNotAliasTheReceiversSlice(t *testing.T) {
	t.Parallel()
	backing := make([]string, 1, 4)
	backing[0] = "kept.go"
	plan := Plan{Kind: ingest.KindIncremental, ReparseFiles: backing}

	got := plan.WithRetryPaths([]string{"retried.go"})

	assert.Equal(t, []string{"kept.go"}, plan.ReparseFiles, "the original plan must be unchanged")
	assert.Equal(t, []string{"kept.go", "retried.go"}, got.ReparseFiles)
	assert.Equal(t, 1, len(backing[:1]), "sanity: the spare capacity is what made the aliasing possible")
}
