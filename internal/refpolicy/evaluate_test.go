package refpolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// registeredBranch builds a WorkBranchPolicyStoreMock that resolves exactly
// one (repoName, branchName) pair to wb, and reports every other lookup as
// workbranchstore.ErrNotFound -- the same "known name resolves, anything
// else is absence" shape internal/handler/git's own enrolledRepoStore test
// fixture uses.
func registeredBranch(repoName, branchName string, wb workbranchstore.WorkBranch) *WorkBranchPolicyStoreMock {
	return &WorkBranchPolicyStoreMock{
		GetWorkBranchFunc: func(_ context.Context, gotRepo, gotBranch string) (workbranchstore.WorkBranch, error) {
			if gotRepo == repoName && gotBranch == branchName {
				return wb, nil
			}
			return workbranchstore.WorkBranch{}, fmt.Errorf("branch %s/%s: %w", gotRepo, gotBranch, workbranchstore.ErrNotFound)
		},
	}
}

// TestEvaluatePush_TableDriven is loam-ofg.18's own Definition of Done: a
// table-driven test of EvaluatePush (not the socket transport) covering
// every one of docs/git-spec.md "Ref Policy (push)"'s four rejection
// reasons, both terminal states rule 3 covers, the allowed path, and
// fail-closed behavior when the store errors or a caller-supplied context
// has already expired.
func TestEvaluatePush_TableDriven(t *testing.T) {
	t.Parallel()
	const repoName = "acme/widgets"
	// The identity is seeded as a whole, and the fixture rows below store
	// its Identifier() -- exactly what internal/handler/workbranch writes
	// into work_branches.author at CreateWorkBranch time. This file used
	// to seed the BARE name, which is why the suite agreed with itself
	// and disagreed with production (loam-ppb).
	agent := httpauth.Identity{Name: "alice", ID: "1", Role: "author"}
	draftOwnedByAlice := workbranchstore.WorkBranch{Name: "wb-owned", Author: agent.Identifier(), State: workbranchstore.StateDraft}
	expiredCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	tests := []struct {
		name        string
		ctx         context.Context
		store       WorkBranchPolicyStore
		update      RefUpdate
		wantErr     bool
		wantAllowed bool
		wantReason  string // substring wantReason must appear in, when not allowed
	}{
		{
			name:        "allowed: author pushing to their own non-terminal work branch",
			ctx:         t.Context(),
			store:       registeredBranch(repoName, "wb-owned", draftOwnedByAlice),
			update:      RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-owned"},
			wantAllowed: true,
		},
		{
			name:        "rejection 1: read-only ref -- update outside refs/heads/ entirely",
			ctx:         t.Context(),
			store:       registeredBranch(repoName, "wb-owned", draftOwnedByAlice),
			update:      RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/tags/v1"},
			wantAllowed: false,
			wantReason:  "loam: refs/tags/v1 is read-only (target branch)",
		},
		{
			name:        "rejection 1: read-only ref -- update to an existing mirrored/target branch",
			ctx:         t.Context(),
			store:       registeredBranch(repoName, "wb-owned", draftOwnedByAlice),
			update:      RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/main"},
			wantAllowed: false,
			wantReason:  "loam: refs/heads/main is read-only (target branch)",
		},
		{
			name:        "rejection 2: unknown ref -- creating a brand-new, unregistered branch name",
			ctx:         t.Context(),
			store:       registeredBranch(repoName, "wb-owned", draftOwnedByAlice),
			update:      RefUpdate{OldSHA: strings.Repeat("0", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/foo"},
			wantAllowed: false,
			wantReason:  "loam: refs/heads/foo is not a work branch; create one with 'work start'",
		},
		{
			name:        "rejection 2: unknown ref -- creating an unregistered name INSIDE the reserved namespace",
			ctx:         t.Context(),
			store:       registeredBranch(repoName, "wb-owned", draftOwnedByAlice),
			update:      RefUpdate{OldSHA: strings.Repeat("0", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/foo"},
			wantAllowed: false,
			wantReason:  "loam: refs/heads/loam-reserved/foo is not a work branch; create one with 'work start'",
		},
		{
			// The reserved namespace is never mirrored from upstream
			// (refnames.ReservedExclusionRefspec), so an UPDATE to an
			// unregistered ref there is not "read-only, owned by upstream"
			// the way refs/heads/<something> would be -- it is the same
			// unknown ref, whichever way its old-sha reads.
			name:        "rejection 2: unknown ref -- UPDATING an unregistered name inside the reserved namespace",
			ctx:         t.Context(),
			store:       registeredBranch(repoName, "wb-owned", draftOwnedByAlice),
			update:      RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/foo"},
			wantAllowed: false,
			wantReason:  "loam: refs/heads/loam-reserved/foo is not a work branch; create one with 'work start'",
		},
		{
			// The shape `git push origin HEAD`, and every push from a
			// clone `loam clone` never bootstrapped, produces: a REAL
			// work branch aimed at the unreserved ref path. Answering it
			// with the generic "create one with 'work start'" would tell
			// an agent who already ran work start to run it again.
			name:        "rejection 1: a registered work branch pushed to the UNRESERVED ref path",
			ctx:         t.Context(),
			store:       registeredBranch(repoName, "wb-owned", draftOwnedByAlice),
			update:      RefUpdate{OldSHA: strings.Repeat("0", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/wb-owned"},
			wantAllowed: false,
			wantReason:  "loam: wb-owned must be pushed to refs/heads/loam-reserved/wb-owned; re-run 'loam clone' to configure the push refspec, then push by branch name",
		},
		{
			name: "rejection 3: not the author",
			ctx:  t.Context(),
			store: registeredBranch(repoName, "wb-owned", workbranchstore.WorkBranch{
				Name: "wb-owned", Author: "grace-hopper-3-author", State: workbranchstore.StateDraft,
			}),
			update:      RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-owned"},
			wantAllowed: false,
			wantReason:  "loam: wb-owned belongs to grace-hopper-3-author",
		},
		{
			name: "rejection 4: terminal state -- closed",
			ctx:  t.Context(),
			store: registeredBranch(repoName, "wb-owned", workbranchstore.WorkBranch{
				Name: "wb-owned", Author: agent.Identifier(), State: workbranchstore.StateClosed,
			}),
			update:      RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-owned"},
			wantAllowed: false,
			wantReason:  "loam: wb-owned is closed",
		},
		{
			name: "rejection 4: terminal state -- complete",
			ctx:  t.Context(),
			store: registeredBranch(repoName, "wb-owned", workbranchstore.WorkBranch{
				Name: "wb-owned", Author: agent.Identifier(), State: workbranchstore.StateComplete,
			}),
			update:      RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-owned"},
			wantAllowed: false,
			wantReason:  "loam: wb-owned is complete",
		},
		{
			name: "allowed: reviewable and reviewed are non-terminal too",
			ctx:  t.Context(),
			store: registeredBranch(repoName, "wb-owned", workbranchstore.WorkBranch{
				Name: "wb-owned", Author: agent.Identifier(), State: workbranchstore.StateReviewable,
			}),
			update:      RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-owned"},
			wantAllowed: true,
		},
		{
			name: "fail-closed: store returns an error other than ErrNotFound",
			ctx:  t.Context(),
			store: &WorkBranchPolicyStoreMock{
				GetWorkBranchFunc: func(context.Context, string, string) (workbranchstore.WorkBranch, error) {
					return workbranchstore.WorkBranch{}, errors.New("connection refused")
				},
			},
			update:  RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-owned"},
			wantErr: true,
		},
		{
			name: "fail-closed: an already-expired context (standing in for a socket read timeout) surfaces as an error",
			ctx:  expiredCtx,
			store: &WorkBranchPolicyStoreMock{
				GetWorkBranchFunc: func(ctx context.Context, _, _ string) (workbranchstore.WorkBranch, error) {
					// A real Postgres call made with an expired context fails
					// with ctx.Err() (context.DeadlineExceeded) rather than
					// completing -- this mock stands in for that, rather than
					// actually sleeping past a deadline, to keep the test
					// deterministic and fast.
					return workbranchstore.WorkBranch{}, ctx.Err()
				},
			},
			update:  RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-owned"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			verdicts, allAllowed, err := EvaluatePush(tt.ctx, tt.store, repoName, agent, []RefUpdate{tt.update}, nil)
			if tt.wantErr {
				require.Error(t, err, "fail-closed case must surface a hard error, not a per-ref verdict")
				assert.Nil(t, verdicts, "a hard evaluation error must not also return a partial verdict list a caller could mistake for a real decision")
				assert.False(t, allAllowed)
				return
			}
			require.NoError(t, err)
			require.Len(t, verdicts, 1)
			assert.Equal(t, tt.wantAllowed, verdicts[0].Allowed)
			assert.Equal(t, tt.wantAllowed, allAllowed)
			if tt.wantAllowed {
				assert.Empty(t, verdicts[0].Reason)
				return
			}
			assert.True(t, strings.HasPrefix(verdicts[0].Reason, "loam: "), "every rejection reason must be loam:-prefixed, got %q", verdicts[0].Reason)
			assert.Equal(t, tt.wantReason, verdicts[0].Reason)
		})
	}
}

// TestEvaluatePush_AtomicRejection proves the atomicity docs/git-spec.md
// "Ref Policy (push)" demands: a push with several refs, only one of which
// fails policy, must report allAllowed == false for the WHOLE push (not
// merely flag the one bad ref while treating the others as independently
// accepted) -- this is the exact "accept when only ONE of several refs
// fails" mutation this bead's own instructions call out.
func TestEvaluatePush_AtomicRejection(t *testing.T) {
	t.Parallel()
	const repoName = "acme/widgets"
	// The identity is seeded as a whole, and the fixture rows below store
	// its Identifier() -- exactly what internal/handler/workbranch writes
	// into work_branches.author at CreateWorkBranch time. This file used
	// to seed the BARE name, which is why the suite agreed with itself
	// and disagreed with production (loam-ppb).
	agent := httpauth.Identity{Name: "alice", ID: "1", Role: "author"}
	store := registeredBranch(repoName, "wb-good", workbranchstore.WorkBranch{Name: "wb-good", Author: agent.Identifier(), State: workbranchstore.StateDraft})
	goodUpdate := RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-good"}
	badUpdate := RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/main"} // not a registered work branch

	// Both orderings are exercised deliberately: an aggregation bug that
	// just remembers the LAST verdict seen (rather than ORing every
	// failure together) would pass a "bad ref last" push by coincidence --
	// the last-seen verdict happens to be the rejecting one -- while still
	// failing to reject a "bad ref first, good ref last" push, where the
	// last-seen verdict is the ALLOWED one. Only checking both orders
	// actually proves every ref's verdict is honored, not just the final
	// one evaluated.
	t.Run("bad ref last", func(t *testing.T) {
		t.Parallel()
		verdicts, allAllowed, err := EvaluatePush(t.Context(), store, repoName, agent, []RefUpdate{goodUpdate, badUpdate}, nil)
		require.NoError(t, err)
		require.Len(t, verdicts, 2)
		assert.True(t, verdicts[0].Allowed, "the individually-fine ref is still reported allowed on its own verdict")
		assert.False(t, verdicts[1].Allowed)
		assert.False(t, allAllowed, "one failing ref must reject the WHOLE push, not just itself")
	})

	t.Run("bad ref first", func(t *testing.T) {
		t.Parallel()
		verdicts, allAllowed, err := EvaluatePush(t.Context(), store, repoName, agent, []RefUpdate{badUpdate, goodUpdate}, nil)
		require.NoError(t, err)
		require.Len(t, verdicts, 2)
		assert.False(t, verdicts[0].Allowed)
		assert.True(t, verdicts[1].Allowed, "the individually-fine ref is still reported allowed on its own verdict")
		assert.False(t, allAllowed, "one failing ref anywhere in the push must reject the WHOLE push, even when every later ref is fine")
	})
}

// TestEvaluatePush_PostAcceptOnlyFiresWhenTheWholePushIsAccepted proves
// PostAcceptFunc -- loam-giq.6's exposed seam -- is never invoked for any
// ref in a push that was atomically rejected, even for the ref that
// individually looked fine; and that it fires exactly once per ref, with
// the WorkBranch row EvaluatePush already fetched, when the whole push is
// accepted.
func TestEvaluatePush_PostAcceptOnlyFiresWhenTheWholePushIsAccepted(t *testing.T) {
	t.Parallel()
	const repoName = "acme/widgets"
	// The identity is seeded as a whole, and the fixture rows below store
	// its Identifier() -- exactly what internal/handler/workbranch writes
	// into work_branches.author at CreateWorkBranch time. This file used
	// to seed the BARE name, which is why the suite agreed with itself
	// and disagreed with production (loam-ppb).
	agent := httpauth.Identity{Name: "alice", ID: "1", Role: "author"}
	goodWB := workbranchstore.WorkBranch{Name: "wb-good", Author: agent.Identifier(), State: workbranchstore.StateDraft}
	store := registeredBranch(repoName, "wb-good", goodWB)

	t.Run("mixed push: onAccept never called", func(t *testing.T) {
		t.Parallel()
		var calls []RefUpdate
		onAccept := func(_ context.Context, _ workbranchstore.WorkBranch, update RefUpdate) { calls = append(calls, update) }
		updates := []RefUpdate{
			{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-good"},
			{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/main"},
		}
		_, allAllowed, err := EvaluatePush(t.Context(), store, repoName, agent, updates, onAccept)
		require.NoError(t, err)
		require.False(t, allAllowed)
		assert.Empty(t, calls, "onAccept must not fire for any ref in an atomically-rejected push")
	})

	t.Run("fully accepted push: onAccept fires once per ref with the fetched row", func(t *testing.T) {
		t.Parallel()
		var calls []RefUpdate
		var gotWB []workbranchstore.WorkBranch
		onAccept := func(_ context.Context, wb workbranchstore.WorkBranch, update RefUpdate) {
			calls = append(calls, update)
			gotWB = append(gotWB, wb)
		}
		updates := []RefUpdate{{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-good"}}
		_, allAllowed, err := EvaluatePush(t.Context(), store, repoName, agent, updates, onAccept)
		require.NoError(t, err)
		require.True(t, allAllowed)
		require.Len(t, calls, 1)
		assert.Equal(t, updates[0], calls[0])
		require.Len(t, gotWB, 1)
		assert.Equal(t, goodWB, gotWB[0], "onAccept must receive the WorkBranch row EvaluatePush already fetched, not force loam-giq.6 to re-query it")
	})

	t.Run("nil onAccept is a safe no-op on a fully accepted push", func(t *testing.T) {
		t.Parallel()
		updates := []RefUpdate{{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-good"}}
		assert.NotPanics(t, func() {
			_, allAllowed, err := EvaluatePush(t.Context(), store, repoName, agent, updates, nil)
			require.NoError(t, err)
			require.True(t, allAllowed)
		})
	})
}

// TestEvaluatePush_EmptyPushIsVacuouslyAllowed proves a push with zero ref
// updates (not a realistic git push, but a defensive edge case) neither
// panics nor rejects: there is nothing to reject.
func TestEvaluatePush_EmptyPushIsVacuouslyAllowed(t *testing.T) {
	t.Parallel()
	store := &WorkBranchPolicyStoreMock{
		GetWorkBranchFunc: func(context.Context, string, string) (workbranchstore.WorkBranch, error) {
			t.Fatal("must not be called for an empty push")
			return workbranchstore.WorkBranch{}, nil
		},
	}
	verdicts, allAllowed, err := EvaluatePush(t.Context(), store, "acme/widgets", httpauth.Identity{Name: "alice", ID: "1", Role: "author"}, nil, nil)
	require.NoError(t, err)
	assert.True(t, allAllowed)
	assert.Empty(t, verdicts)
}

// TestEvaluatePush_EmptyAgentNameNeverMatchesAnEmptyAuthor proves the
// review-caught defense-in-depth gap: work_branches.author is NOT NULL
// but does not forbid an empty string, so a plain "!=" author check would
// let an unset LOAM_AGENT_NAME (agentName == "") incorrectly "match" a
// row whose author somehow also reads back empty. This never happens
// through the real request path (internal/httpauth.GitIdentity 403s a
// missing identity before receive-pack runs at all), but EvaluatePush's
// OWN contract must not silently accept it either.
func TestEvaluatePush_EmptyAgentNameNeverMatchesAnEmptyAuthor(t *testing.T) {
	t.Parallel()
	const repoName = "acme/widgets"
	store := registeredBranch(repoName, "wb-owned", workbranchstore.WorkBranch{Name: "wb-owned", Author: "", State: workbranchstore.StateDraft})
	update := RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-owned"}
	verdicts, allAllowed, err := EvaluatePush(t.Context(), store, repoName, httpauth.Identity{}, []RefUpdate{update}, nil)
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	assert.False(t, allAllowed)
	assert.False(t, verdicts[0].Allowed, "an empty agentName must never be treated as a matching author, even against a row whose author also reads back empty")
	assert.True(t, strings.HasPrefix(verdicts[0].Reason, "loam: "))
}

// TestEvaluatePush_ZeroIdentityNeverMatchesItsOwnRendering is the case the
// parts-emptiness guard actually exists for, and the reason the guard
// checks agent.Name/ID/Role rather than the rendered identifier.
//
// A zero httpauth.Identity renders as "--" (two separator dashes and three
// empty parts), NOT as "". So a row whose author literally reads back "--"
// would be matched by a plain `wb.Author != agent.Identifier()` check, and
// an unset identity would be accepted as that row's author. Its sibling
// test above cannot catch this: it seeds author = "", which "--" never
// equals, so the inequality rejects it regardless of whether the guard is
// present. Verified by mutation -- deleting the parts check leaves that
// test green and only turns this one red.
//
// work_branches.author is NOT NULL but does not forbid "--", and in
// production internal/httpauth.GitIdentity 403s a missing identity before
// receive-pack runs at all, so this is defense in depth for EvaluatePush's
// own contract rather than a reachable production path.
func TestEvaluatePush_ZeroIdentityNeverMatchesItsOwnRendering(t *testing.T) {
	t.Parallel()
	const repoName = "acme/widgets"
	zero := httpauth.Identity{}
	require.Equal(t, "--", zero.Identifier(), "precondition: a zero identity renders as \"--\", which is what makes this row's author matchable")
	store := registeredBranch(repoName, "wb-owned", workbranchstore.WorkBranch{Name: "wb-owned", Author: zero.Identifier(), State: workbranchstore.StateDraft})
	update := RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-owned"}
	verdicts, allAllowed, err := EvaluatePush(t.Context(), store, repoName, zero, []RefUpdate{update}, nil)
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	assert.False(t, allAllowed)
	assert.False(t, verdicts[0].Allowed, "an identity with empty parts must be rejected even against a row whose author equals its rendering")
}

// TestEvaluatePush_UnrecognizedStateFailsClosed proves rule 3's
// allowlist-not-denylist shape: a State this package does not recognize
// as one of the three explicitly pushable values (draft/reviewable/
// reviewed) must be rejected, not silently treated as "not one of the two
// terminal states, so it must be fine." This is what protects against a
// future sixth work_branches.state value added to the schema without
// this function being updated to match.
func TestEvaluatePush_UnrecognizedStateFailsClosed(t *testing.T) {
	t.Parallel()
	const repoName = "acme/widgets"
	// The identity is seeded as a whole, and the fixture rows below store
	// its Identifier() -- exactly what internal/handler/workbranch writes
	// into work_branches.author at CreateWorkBranch time. This file used
	// to seed the BARE name, which is why the suite agreed with itself
	// and disagreed with production (loam-ppb).
	agent := httpauth.Identity{Name: "alice", ID: "1", Role: "author"}
	store := registeredBranch(repoName, "wb-owned", workbranchstore.WorkBranch{
		Name: "wb-owned", Author: agent.Identifier(), State: workbranchstore.State("some-future-state"),
	})
	update := RefUpdate{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/loam-reserved/wb-owned"}
	verdicts, allAllowed, err := EvaluatePush(t.Context(), store, repoName, agent, []RefUpdate{update}, nil)
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	assert.False(t, allAllowed)
	assert.False(t, verdicts[0].Allowed, "a State outside the explicit draft/reviewable/reviewed allowlist must fail closed")
}
