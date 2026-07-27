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

	"github.com/bobcob7/loam/internal/workbranchstore"
)

//go:generate go tool moq -out moq_test.go . WorkBranchStore

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
