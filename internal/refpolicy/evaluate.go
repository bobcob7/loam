package refpolicy

import (
	"context"
	"errors"
	"fmt"

	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/refnames"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// zeroChar is the character git's own ref-update wire format uses to fill
// an old-sha or new-sha value that names "no object" -- an all-zero SHA
// (40 hex digits for SHA-1, 64 for SHA-256) on old_sha means "this ref
// does not exist yet, this update is a CREATE" (git's own convention, not
// anything docs/git-spec.md states -- confirmed against git's own
// receive-pack behavior during this bead's research). EvaluatePush uses
// this, not any Postgres lookup, to tell "unknown ref" (a create) apart
// from "read-only ref" (an update to an existing, non-work-branch ref) for
// a name GetWorkBranch reports as not found: an update can only reach an
// existing ref, and the only refs that exist in a bare mirror besides
// registered work branches are the mirrored/upstream ones docs/git-spec.md
// "Ref Policy (push)" calls "read-only ... owned by upstream sync."
const zeroChar = '0'

// RefUpdate is one line of a pre-receive hook's own stdin: git feeds
// exactly this shape, one line per proposed ref in the push, for the
// WHOLE push in one hook invocation (docs/git-spec.md "Enforcement
// Mechanics": "a trivial stub that forwards the proposed ref updates ...
// over a unix socket" -- the wire encoding of this struct is
// internal/hooksocket's concern, not this package's).
type RefUpdate struct {
	OldSHA string
	NewSHA string
	Ref    string
}

// RefVerdict is EvaluatePush's per-ref decision. Reason is only meaningful
// when Allowed is false, and is always "loam:"-prefixed per docs/git-
// spec.md "Ref Policy (push)"'s reason table -- the exact string a caller
// (internal/hooksocket's server, ultimately the hook process) surfaces to
// the pushing agent.
type RefVerdict struct {
	Ref     string
	Allowed bool
	Reason  string
}

// PostAcceptFunc is invoked once per accepted ref update, AFTER every ref
// in the push has passed policy (never for a push atomically rejected as a
// whole), given the WorkBranch row EvaluatePush already fetched from
// Postgres to make that ref's own decision. Its consumer is catch-up
// detection (loam-giq.6: clearing a work branch's conflict flag when a
// push brings it up to the current target tip), which needs exactly that
// row -- both to decide whether the new tip contains the target and,
// because the row is this push's PRE-push view of the branch, to tell a
// demoted branch from a merely flagged one. Reusing the row this function
// already fetched means that consumer never needs a duplicate "look up
// this work branch" Postgres query.
//
// This is deliberately the NARROW half of the seam: internal/hooksocket
// wraps it in its own PostAcceptFunc, widening it with the two per-PUSH
// facts this transport-free package has no business carrying (the repo
// name and receive-pack's object quarantine directory), and it is that
// wider seam production wires to internal/catchup. A nil hook here is a
// documented no-op, which is what hooksocket passes when it has no
// post-accept consumer of its own.
type PostAcceptFunc func(ctx context.Context, wb workbranchstore.WorkBranch, update RefUpdate)

// EvaluatePush is docs/git-spec.md "Ref Policy (push)"'s three rules,
// evaluated as one Go function against every ref update in a single push,
// atomically: it returns allAllowed = true only if EVERY update in updates
// independently satisfies all three rules. verdicts always has exactly
// len(updates) entries, in the same order, whether or not the whole push
// is allowed -- a caller (internal/hooksocket) reports one loam:-prefixed
// line per FAILING verdict, and, per docs/git-spec.md's own atomicity
// requirement ("one bad ref update rejects the whole push"), must reject
// every ref in the push the instant allAllowed is false, not just the ones
// individually flagged.
//
// A non-nil error means evaluation itself could not complete for reasons
// unrelated to policy -- a store error, or ctx's deadline expiring mid-
// lookup (ctx is threaded into every store call, so a caller-supplied
// timeout surfaces here as a wrapped context.DeadlineExceeded) -- and the
// caller MUST treat that identically to allAllowed == false: reject the
// whole push. This is what makes EvaluatePush fail-closed on
// infrastructure failure, not just on a genuine policy violation: verdicts
// is nil whenever err is non-nil, so there is no partial verdict list a
// caller could mistakenly treat as a real (if incomplete) decision.
//
// onAccept is invoked once per update, in order, only after every update
// has been confirmed allowed -- never for a push where some other ref
// failed -- so a partially-evaluated or atomically-rejected push never
// triggers loam-giq.6's future catch-up bookkeeping for the refs that
// individually looked fine.
func EvaluatePush(ctx context.Context, store WorkBranchPolicyStore, repoName string, agent httpauth.Identity, updates []RefUpdate, onAccept PostAcceptFunc) (verdicts []RefVerdict, allAllowed bool, err error) {
	verdicts = make([]RefVerdict, len(updates))
	workBranches := make([]workbranchstore.WorkBranch, len(updates))
	allAllowed = true
	for i, update := range updates {
		verdict, wb, evalErr := evaluateOne(ctx, store, repoName, agent, update)
		if evalErr != nil {
			return nil, false, fmt.Errorf("evaluating push to %s for ref %s: %w", repoName, update.Ref, evalErr)
		}
		verdicts[i] = verdict
		workBranches[i] = wb
		if !verdict.Allowed {
			allAllowed = false
		}
	}
	if allAllowed && onAccept != nil {
		for i, update := range updates {
			onAccept(ctx, workBranches[i], update)
		}
	}
	return verdicts, allAllowed, nil
}

// evaluateOne applies docs/git-spec.md "Ref Policy (push)"'s three rules to
// a single ref update, returning the WorkBranch row it fetched (zero value
// if the ref was rejected before any row existed to fetch) so EvaluatePush
// can hand it to onAccept without a second Postgres round trip. A non-nil
// error here means the store call itself failed for a reason other than
// "no such work branch" (workbranchstore.ErrNotFound) -- infrastructure
// failure, not a policy verdict -- and EvaluatePush propagates it as a
// hard evaluation error rather than folding it into a per-ref Reason
// string, so the whole push fails closed rather than one ref quietly
// reporting a misleading rejection reason.
func evaluateOne(ctx context.Context, store WorkBranchPolicyStore, repoName string, agent httpauth.Identity, update RefUpdate) (RefVerdict, workbranchstore.WorkBranch, error) {
	branchName, isWorkBranchRef := refnames.WorkBranchName(update.Ref)
	if !isWorkBranchRef {
		return outsideReservedNamespaceVerdict(ctx, store, repoName, update)
	}
	wb, err := store.GetWorkBranch(ctx, repoName, branchName)
	if err != nil {
		if errors.Is(err, workbranchstore.ErrNotFound) {
			return unknownRefVerdict(update.Ref), workbranchstore.WorkBranch{}, nil
		}
		return RefVerdict{}, workbranchstore.WorkBranch{}, fmt.Errorf("looking up work branch %s: %w", branchName, err)
	}
	// The comparison is against agent.Identifier() -- the "<name>-<id>-
	// <role>" rendering -- because that is exactly what
	// work_branches.author holds: internal/handler/workbranch's
	// authorIdentifier() stores httpauth.Identity.Identifier() at
	// CreateWorkBranch time, and docs/persistence-spec.md calls the column
	// an "agent identifier" (and says author/reviewer are "stored as
	// identifier strings"). This function used to compare the row against
	// the BARE agent name, which can only match when a name happens to be
	// identifier-shaped -- so in production an author could never push to
	// the work branch they had just started, and the rejection named the
	// pushing agent as the owner it was refusing (loam-ppb). Every fixture
	// in the tree seeded author as a bare name, so the suite agreed with
	// itself and disagreed with production; those fixtures were corrected
	// alongside this change.
	//
	// The emptiness guard checks the identity's PARTS, not the rendered
	// string, and that distinction is load-bearing: a zero Identity
	// renders as "--", not "", so a rendered-string check would let an
	// unset identity through against a row whose author somehow also read
	// back as "--". work_branches.author is NOT NULL but does not forbid
	// an empty string. In production internal/httpauth.GitIdentity already
	// 403s a request with no identity before receive-pack ever runs, so
	// reaching this path at all needs that middleware bypassed -- but that
	// is precisely why this local check exists: defense in depth for this
	// function's OWN contract, not a rule that depends on some other
	// package's gate always running first.
	if agent.Name == "" || agent.ID == "" || agent.Role == "" || wb.Author != agent.Identifier() {
		return RefVerdict{Ref: update.Ref, Allowed: false, Reason: fmt.Sprintf("loam: %s belongs to %s", wb.Name, wb.Author)}, wb, nil
	}
	if !isNonTerminal(wb.State) {
		// The exact wording here ("is closed") is docs/git-spec.md's own
		// pinned example (its reason table's one terminal-state row); "is
		// complete" for the OTHER terminal state is this package's own
		// generalization of that same template, not a string the spec
		// itself states -- docs/git-spec.md never shows a "complete"
		// example, only "closed".
		return RefVerdict{Ref: update.Ref, Allowed: false, Reason: fmt.Sprintf("loam: %s is %s", wb.Name, wb.State)}, wb, nil
	}
	return RefVerdict{Ref: update.Ref, Allowed: true}, wb, nil
}

// outsideReservedNamespaceVerdict decides every ref that is NOT under
// refnames.ReservedNamespace, which since loam-cmq is every ref a push can
// name except a work branch's own: a mirrored target branch, some other
// refs/heads/ name, or a ref outside refs/heads/ entirely.
//
// The one case worth spending a store lookup on is a push aimed at
// refs/heads/<name> where <name> IS a registered work branch of this repo.
// That is not a hypothetical: agents push with plain git, and `git push
// origin HEAD` resolves its destination by name and lands on
// refs/heads/wb-9c2f1a regardless of remote.origin.push (verified against
// real git 2.50.1), as does any push from a clone that `loam clone` never
// bootstrapped. Answering that with the generic "is not a work branch;
// create one with 'work start'" would tell an agent who DID run work start
// to run it again -- so it gets its own reason naming the ref that would
// have worked. Every other ref keeps the reasons docs/git-spec.md's table
// already pins.
func outsideReservedNamespaceVerdict(ctx context.Context, store WorkBranchPolicyStore, repoName string, update RefUpdate) (RefVerdict, workbranchstore.WorkBranch, error) {
	branchName, isBranch := refnames.BranchName(update.Ref)
	if !isBranch {
		return readOnlyVerdict(update.Ref), workbranchstore.WorkBranch{}, nil
	}
	wb, err := store.GetWorkBranch(ctx, repoName, branchName)
	if err != nil {
		if errors.Is(err, workbranchstore.ErrNotFound) {
			return unknownOrReadOnlyVerdict(update), workbranchstore.WorkBranch{}, nil
		}
		return RefVerdict{}, workbranchstore.WorkBranch{}, fmt.Errorf("looking up work branch %s: %w", branchName, err)
	}
	// The WorkBranch row is deliberately NOT returned to the caller here:
	// this verdict is a rejection, and EvaluatePush only ever hands
	// onAccept rows for updates it allowed.
	return misplacedWorkBranchVerdict(update.Ref, wb.Name), workbranchstore.WorkBranch{}, nil
}

// readOnlyVerdict is rule 1's rejection for any ref outside refs/heads/
// entirely (docs/git-spec.md "Ref Policy (push)": "any update outside
// refs/heads/ ... rejected"; "Mirrored refs ... read-only to agents").
func readOnlyVerdict(ref string) RefVerdict {
	return RefVerdict{Ref: ref, Allowed: false, Reason: fmt.Sprintf("loam: %s is read-only (target branch)", ref)}
}

// unknownRefVerdict is rule 1's rejection for a ref inside
// refnames.ReservedNamespace that names no registered work branch. Unlike
// unknownOrReadOnlyVerdict below it needs no create/update discrimination:
// the reserved namespace is server-owned and is never mirrored from
// upstream (refnames.ReservedExclusionRefspec removes the whole subtree
// from every fetch), so a ref there is either a registered work branch --
// handled by the caller, before this is reached -- or something that
// should not exist, whichever way its old-sha reads.
func unknownRefVerdict(ref string) RefVerdict {
	return RefVerdict{Ref: ref, Allowed: false, Reason: fmt.Sprintf("loam: %s is not a work branch; create one with 'work start'", ref)}
}

// misplacedWorkBranchVerdict rejects a push that names a real, registered
// work branch at the WRONG ref path -- refs/heads/<name> rather than
// refnames.WorkBranch(<name>). The reason names the ref that would have
// worked and the one command that reconfigures a clone to produce it,
// because the cause is always a clone whose remote.origin.push
// refnames.ClientPushRefspec never reached (a hand-rolled clone) or a push
// that bypassed it (`git push origin HEAD`).
func misplacedWorkBranchVerdict(ref, name string) RefVerdict {
	return RefVerdict{
		Ref:     ref,
		Allowed: false,
		Reason:  fmt.Sprintf("loam: %s must be pushed to %s; re-run 'loam clone' to configure the push refspec, then push by branch name", name, refnames.WorkBranch(name)),
	}
}

// unknownOrReadOnlyVerdict picks between rule 1's two reasons for a
// refs/heads/<name> ref that names no registered work branch, using
// update.OldSHA's all-zero-ness (zeroChar's own doc comment) to tell a
// brand-new ref creation ("unknown ref": docs/git-spec.md's example is
// literally "create one with 'work start'") apart from a push updating a
// ref that already exists in the mirror, which given the only refs a bare
// mirror holds outside the reserved namespace are upstream/target refs,
// must be the latter ("read-only ref": "<ref> is read-only (target
// branch)").
func unknownOrReadOnlyVerdict(update RefUpdate) RefVerdict {
	if isZeroSHA(update.OldSHA) {
		return unknownRefVerdict(update.Ref)
	}
	return readOnlyVerdict(update.Ref)
}

// isZeroSHA reports whether sha is git's all-zero "no object" sentinel --
// every character '0', and at least one character present, so an empty
// string (which is never a real ref update in practice, but should not be
// misclassified as a create) reports false.
func isZeroSHA(sha string) bool {
	if sha == "" {
		return false
	}
	for _, r := range sha {
		if r != zeroChar {
			return false
		}
	}
	return true
}

// isNonTerminal reports whether state is one of the three states
// docs/git-spec.md "Ref Policy (push)" rule 3 names POSITIVELY as
// pushable -- "draft / reviewable / reviewed" -- rather than checking the
// negative (state is complete or closed). This is an explicit allowlist,
// not a denylist, so it fails CLOSED on anything it does not recognize:
// an empty State (a zero-value bug elsewhere), a typo, or a future sixth
// work_branches.state value added to the schema without this function
// being updated all report false (not pushable) here, rather than
// silently falling through a denylist's implicit "anything else is fine."
func isNonTerminal(state workbranchstore.State) bool {
	return state == workbranchstore.StateDraft || state == workbranchstore.StateReviewable || state == workbranchstore.StateReviewed
}
