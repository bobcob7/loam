// Package hooksocket implements the unix-socket transport docs/server-
// spec.md's Process Model names ("Policy socket (<data>/hook.sock) --
// the unix socket serving pre-receive ref-policy decisions to the hook
// stubs in the mirrors") and docs/git-spec.md "Enforcement Mechanics"
// describes ("a trivial stub that forwards the proposed ref updates ...
// to the server over a unix socket, and passes or fails on the answer").
// It wraps internal/refpolicy.EvaluatePush -- the actual rule
// evaluation -- in exactly one JSON request/response round trip per
// connection; Server is the listener side cmd/server runs, and Call (in
// client.go) is the dial-and-round-trip helper cmd/loamhook (the hook
// process itself) and this package's own tests both use, so the wire
// format is exercised the same way from both directions.
package hooksocket

import (
	"context"

	"github.com/bobcob7/loam/internal/refpolicy"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

//go:generate go tool moq -out moq_test.go . WorkBranchStore

// AcceptedPush is one accepted ref update, handed to PostAcceptFunc after
// the WHOLE push has passed ref policy. It is this package's own widening
// of internal/refpolicy.PostAcceptFunc's arguments, and it is defined HERE
// rather than in refpolicy because two of its four fields are things
// refpolicy deliberately does not know about: refpolicy is transport-free
// by its own package doc comment, and both Repo and QuarantineDir come off
// this package's wire Request rather than out of any policy rule.
//
// WorkBranch is the row refpolicy.EvaluatePush already fetched to make
// this ref's own policy decision, passed through so a post-accept consumer
// never needs a duplicate "look up this work branch" query -- and so it
// sees the branch as it was BEFORE this push, which is what
// internal/catchup's demoted-versus-merely-flagged rule is decided on.
//
// QuarantineDir is receive-pack's GIT_QUARANTINE_PATH for this push, as
// the hook process read it out of its own environment. It matters because
// a post-accept consumer runs while the pushed objects are still
// quarantined: a separate `git --git-dir=<mirror>` process cannot see the
// new tip at all without being pointed at that directory (see
// internal/gitancestry's package doc comment for the measurement). It is
// empty when the hook had no quarantine to report -- an older git, or a
// caller driving this package directly -- and a consumer must treat that
// as "nothing extra to read", not as an error.
//
// Like every other field on Request, QuarantineDir is only as trustworthy
// as the socket itself: anything able to dial it could name an arbitrary
// directory. That is not a new exposure -- the same actor could forge the
// repo, the identity, and the ref updates, all of which decide far more --
// and the socket's protection is filesystem permissions on
// <data>/hook.sock, not validation here.
type AcceptedPush struct {
	Repo          string
	QuarantineDir string
	WorkBranch    workbranchstore.WorkBranch
	Update        refpolicy.RefUpdate
}

// PostAcceptFunc is the hook Server invokes once per accepted ref update,
// in order, only after every update in the push has been allowed. It has
// no error return by construction: policy has already accepted the push by
// the time it runs, so there is nothing a failure here could honestly
// undo. Consumers log and move on (internal/catchup.Detector.OnAcceptedPush
// is the production implementation).
//
// A nil PostAcceptFunc is a documented no-op, and is what a caller with no
// post-accept bookkeeping to do passes.
type PostAcceptFunc func(ctx context.Context, accepted AcceptedPush)

// WorkBranchStore is this package's own copy of
// internal/refpolicy.WorkBranchPolicyStore's method set, defined here per
// this repo's "interfaces at the consumer" convention (every package
// consuming this seam defines its own copy, rather than importing
// refpolicy's interface type) rather than importing that interface type
// directly. Go's structural interface assignability means a value held as
// this type still passes straight into refpolicy.EvaluatePush's
// identically-shaped parameter with no adapter or cast -- production
// wiring (cmd/server) supplies the very same concrete adapter to both;
// only the TEST-time mock differs (this package's own moq'd
// WorkBranchStoreMock below, never refpolicy's own, which lives in
// refpolicy's moq_test.go and is not importable from outside that
// package).
type WorkBranchStore interface {
	GetWorkBranch(ctx context.Context, repoName, branchName string) (workbranchstore.WorkBranch, error)
}
