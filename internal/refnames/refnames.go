// Package refnames is the single source of the ref paths Loam's own
// work-branch refs occupy inside a repo's bare mirror, and of the two
// client-side refspecs `loam clone` writes so plain git reaches them.
//
// # Why work-branch refs live under a reserved namespace
//
// docs/git-spec.md -> Ref Policy reserves refs/heads/loam-reserved/ in the
// MIRROR as server-owned. Work-branch refs live there -- refs/heads/loam-
// reserved/wb-9c2f1a -- rather than at refs/heads/wb-9c2f1a, because the
// mirror fetch (internal/mirrorsync.MirrorFetcher) is a forced, PRUNING
// fetch of "+refs/*:refs/*" whose argv, including one negative exclusion
// per currently registered work branch, is fixed before the network
// operation begins. A work branch created at any point during that fetch
// -- seconds to minutes on a large repo -- is not in the exclusion list,
// so its brand-new ref is a prune candidate. Verified against real git
// 2.50.1: a purely-local refs/heads/wb-brandnew that never existed
// upstream is deleted outright by that refspec with --prune. The deletion
// is UNRECOVERABLE -- work_branches carries no SHA column and a bare
// mirror has no reflog -- so the row survives pointing at a ref that no
// longer exists and the agent's commits become gc-able (loam-cmq).
//
// A whole reserved PATH SEGMENT, excluded structurally by
// ReservedExclusionRefspec, closes that window for every work-branch ref
// that will ever exist, including ones created mid-fetch. The enumerated
// per-branch exclusions stay as the semantic rule; this glob is the
// structural backstop, not a replacement.
//
// # Why the namespace is in the REF PATH and not in the NAME
//
// The work branch's NAME is unchanged -- still "wb-9c2f1a", still what the
// CLI takes as its <work-branch> argument, still what upstream sees as
// loam/wb-9c2f1a. Only the mirror ref path carries the namespace. Putting
// it in the name was rejected for three concrete reasons: internal/
// mirrorsync's workBranchNamePattern rejects slashes, so every work branch
// would fail safeWorkBranchName and break the upstream proposal push and
// the upstream ref cleanup; the upstream branch would become the redundant
// loam/loam-reserved/wb-9c2f1a; and every CLI <work-branch> argument would
// carry the prefix.
//
// # The cost this package's client refspecs pay for
//
// Agents push with plain git. `git push origin wb-9c2f1a` normally targets
// refs/heads/wb-9c2f1a -- which is now an unregistered ref that
// internal/refpolicy rejects. ClientPushRefspec and ClientFetchRefspec are
// what `loam clone` writes into remote.origin.push / remote.origin.fetch
// so that same plain-git command reaches the reserved path instead. That
// makes the clone bootstrap load-bearing for pushes: a hand-rolled clone
// no longer pushes anywhere the server accepts. See docs/git-spec.md ->
// "The CLI's Role".
package refnames

import "strings"

// ReservedNamespace is the ref path prefix docs/git-spec.md -> Ref Policy
// reserves in the mirror as server-owned. Everything below it is written
// by Loam itself (today: exactly the work-branch refs) and nothing below
// it is ever mirrored from upstream -- ReservedExclusionRefspec removes
// the whole subtree from the fetch refspec, so an upstream branch that
// happened to be named loam-reserved/<something> is simply not carried
// into the mirror.
//
// A whole path segment, not a bare "wb-" prefix: a segment is far less
// likely to collide with a real upstream branch name, which was the main
// cost of the shorter proposal (loam-cmq's NOTES).
const ReservedNamespace = "refs/heads/loam-reserved/"

// ReservedExclusionRefspec is the negative refspec that removes
// ReservedNamespace's whole subtree from a fetch. git accepts a glob in a
// negative refspec, and a ref the negative refspec removes is never a
// fetch candidate NOR a prune candidate -- verified against real git
// 2.50.1, and pinned by TestBuildFetchRefspecs_ReservedGlobSurvivesRealPrune
// in internal/mirrorsync.
//
// The trailing "*" is a glob, not a prefix match on a partial segment:
// "^refs/heads/loam-reserved/*" excludes refs/heads/loam-reserved/wb-9c2f1a
// and everything else below that path.
const ReservedExclusionRefspec = "^" + ReservedNamespace + "*"

// headsPrefix is the namespace an ordinary branch -- a mirrored target
// branch, or a work branch as plain git names it in an agent's own clone
// -- occupies. Work-branch refs in the MIRROR do not live here directly;
// they live under ReservedNamespace, which is itself below this prefix.
const headsPrefix = "refs/heads/"

// ClientPushRefspec is what `loam clone` writes to remote.origin.push so a
// plain `git push origin wb-9c2f1a` from the clone lands on
// WorkBranch("wb-9c2f1a") instead of refs/heads/wb-9c2f1a.
//
// git-push(1) is explicit that this is what makes a destination-less
// command-line refspec work: "If git push [<repository>] without any
// <refspec> argument is set to update some ref at the destination with
// <src> with remote.<repository>.push configuration variable, :<dst> part
// can be omitted -- such a push will update a ref that <src> normally
// updates without any <refspec> on the command line." Verified against
// real git 2.50.1: with this configured, `git push origin wb-9c2f1a`
// reports "wb-9c2f1a -> loam-reserved/wb-9c2f1a".
//
// The source side is "refs/heads/wb-*", NOT "refs/heads/*", and the
// narrowing is deliberate. With "refs/heads/*" a bare `git push` matches
// EVERY local branch, so the clone's own "main" is swept into the same
// push as refs/heads/loam-reserved/main -- an unregistered ref, which
// refpolicy rejects, which (pre-receive being atomic) rejects the work
// branch's update along with it. Measured: a bare `git push` failed
// outright. Scoping the source to "wb-*" -- the prefix
// internal/handler/workbranch's randomWorkBranchName always generates --
// makes a bare `git push` push only work branches, and leaves an explicit
// `git push origin main` falling back to git's ordinary same-name
// destination, so the target branch still gets refpolicy's
// "read-only (target branch)" rejection rather than a confusing one about
// a reserved path. This is a client-side DEFAULT, the same class of thing
// as `loam clone`'s --single-branch: it configures the common case, it
// does not enforce anything.
const ClientPushRefspec = "refs/heads/wb-*:" + ReservedNamespace + "wb-*"

// ClientFetchRefspec is what `loam clone` ADDS to remote.origin.fetch (via
// `git config --add`, never replacing the single-branch refspec the clone
// itself wrote) so a plain `git fetch` in the clone brings work branches
// down under their BARE names: refs/heads/loam-reserved/wb-9c2f1a becomes
// refs/remotes/origin/wb-9c2f1a, and `git checkout wb-9c2f1a` does the
// obvious thing. Without it a reviewer's clone can see the target branch
// and nothing else, since --single-branch leaves exactly one refspec
// behind.
const ClientFetchRefspec = "+" + ReservedNamespace + "*:refs/remotes/origin/*"

// CloneBranch renders a work branch's name as `git clone --branch` must
// spell it: "loam-reserved/wb-9c2f1a", the ref path with refs/heads/
// stripped. --branch is resolved against the remote's ADVERTISED refs, not
// through remote.origin.fetch, so ClientFetchRefspec does not help here and
// the bare name simply does not resolve (measured against real git 2.50.1:
// "fatal: Remote branch wb-9c2f1a not found in upstream origin"). A full
// "refs/heads/..." path does not resolve either -- --branch takes the
// short form only, also measured.
//
// A clone made this way names its local branch after the argument, so
// `loam clone` renames it back to the bare name afterwards; `git branch -m`
// carries branch.<name>.remote/merge across with it, leaving `git pull`
// tracking the reserved ref and `git push` going through
// ClientPushRefspec.
func CloneBranch(name string) string {
	return strings.TrimPrefix(ReservedNamespace, headsPrefix) + name
}

// WorkBranch returns the mirror ref path for a registered work branch's
// bare name (docs/git-spec.md -> Ref Policy). Callers hand git the full
// ref path rather than the bare name so a same-named tag or remote-
// tracking ref in the mirror can never be resolved instead.
func WorkBranch(name string) string {
	return ReservedNamespace + name
}

// TargetBranch returns the mirror ref path for a target branch's bare name.
// Target branches are mirrored refs and stay exactly where upstream put
// them -- refs/heads/<name> -- so this is NOT WorkBranch's namespace.
func TargetBranch(name string) string {
	return headsPrefix + name
}

// WorkBranchName inverts WorkBranch: it recovers the bare work-branch name
// from a mirror ref path, reporting ok=false for any ref outside
// ReservedNamespace and for the namespace prefix with nothing after it.
func WorkBranchName(ref string) (name string, ok bool) {
	if !strings.HasPrefix(ref, ReservedNamespace) {
		return "", false
	}
	name = strings.TrimPrefix(ref, ReservedNamespace)
	if name == "" {
		return "", false
	}
	return name, true
}

// BranchName recovers the bare branch name from a refs/heads/ ref that is
// NOT under ReservedNamespace -- a mirrored target branch, or an agent's
// push aimed at the unreserved path. ok is false for a ref outside
// refs/heads/ entirely, for one under ReservedNamespace (use
// WorkBranchName for those), and for the bare prefix with nothing after it.
func BranchName(ref string) (name string, ok bool) {
	if !strings.HasPrefix(ref, headsPrefix) || strings.HasPrefix(ref, ReservedNamespace) {
		return "", false
	}
	name = strings.TrimPrefix(ref, headsPrefix)
	if name == "" {
		return "", false
	}
	return name, true
}
