package mirrorsync

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/refnames"
)

// branchesRefspec and tagsRefspec are the two positive refspecs
// MirrorFetcher pairs with a negative exclusion per registered work-branch
// ref (docs/sync-spec.md -> Mirror Sync step 1): every upstream branch and
// every upstream tag, and nothing outside those two namespaces -- no
// refs/pull/*, refs/notes/*, refs/replace/*, or any other upstream ref
// (loam-5f3 narrowed this from the git-clone---mirror-equivalent
// "+refs/*:refs/*" wildcard this constant used to hold single-handedly;
// see buildFetchRefspecs for why). The leading '+' on each is NOT what
// makes a diverged or force-pushed upstream ref win here:
// gittransport.Transport.Fetch already passes the git-level --force flag
// unconditionally, which (per git-fetch(1) -f/--force) overrides the
// non-fast-forward check for every refspec regardless of a per-refspec
// '+' -- dropping the '+' leaves every integration test in this package
// green; only the refspec-string unit tests catch it. The '+' is kept
// anyway because it is what docs/sync-spec.md's DESIGN mandates verbatim
// and it is the same leading '+' `git clone --mirror` itself uses on its
// own (broader) refs/*:refs/* refspec, so intent stays correct even if
// Transport ever stops passing --force: belt-and-suspenders notation, not
// the forcing mechanism. The sharper unconditional flag actually in play
// is --prune, which is safe to run unconditionally here only because git
// scopes pruning to each refspec's own destination namespace -- a ref a
// negative exclusion removes from one of these refspecs is never a prune
// candidate either, exactly as it is never a fetch candidate, and a ref
// outside both destination namespaces entirely (a refs/pull/* ref, say)
// was never a prune candidate to begin with, since neither refspec's
// destination reaches it.
const (
	branchesRefspec = "+refs/heads/*:refs/heads/*"
	tagsRefspec     = "+refs/tags/*:refs/tags/*"
)

// MirrorFetcher is the production Fetcher (docs/sync-spec.md -> Mirror
// Sync step 1; docs/git-spec.md -> Ref Policy "Work-branch refs"; owned by
// bead giq.2): a forced, pruning fetch of every upstream branch and tag
// into a repo's bare mirror, upstream-wins, with every currently
// registered work-branch ref excluded from the refspec so a same-named
// upstream branch can never clobber one of Loam's own. Nothing outside
// refs/heads/* and refs/tags/* is fetched (loam-5f3): no refs/pull/*,
// refs/notes/*, or refs/replace/* -- nothing in loam consumes any of
// those, and refs/replace/* in particular would otherwise silently alter
// object visibility in the mirror, since git applies replace refs
// transparently. The exclusion list is recomputed from work_branches
// immediately before every call to Fetch, since branches come and go
// between sync ticks.
//
// MirrorFetcher builds refspecs and turns gittransport's returned
// --porcelain output back into a FetchResult; it never shells out to git
// itself -- that stays inside upstream (in production,
// *gittransport.Transport), the seam that owns credential injection,
// config isolation, and secret scrubbing (loam-giq.3).
type MirrorFetcher struct {
	dataDir  string
	upstream upstreamRefFetcher
	repos    repoResolver
}

// NewMirrorFetcher builds a MirrorFetcher rooted at dataDir (LOAM_DATA_DIR;
// docs/server-spec.md: "bare mirrors under
// <dir>/mirrors/<group>/<repo_name>.git"), running fetches through
// upstream (in production, *gittransport.Transport) and resolving each
// repo's fetch coordinates and current work-branch names through repos
// (in production, StoreRepoResolver).
func NewMirrorFetcher(dataDir string, upstream upstreamRefFetcher, repos repoResolver) *MirrorFetcher {
	return &MirrorFetcher{dataDir: dataDir, upstream: upstream, repos: repos}
}

// Fetch satisfies Fetcher: it resolves repo's forge host, upstream URL,
// and currently registered work-branch names, builds the refspec set,
// runs the fetch through upstream, and parses the returned --porcelain
// output into a FetchResult.
func (f *MirrorFetcher) Fetch(ctx context.Context, repo RepoID) (FetchResult, error) {
	host, upstreamURL, workBranches, err := f.repos.ResolveRepo(ctx, repo)
	if err != nil {
		return FetchResult{}, fmt.Errorf("resolving repo %s for mirror fetch: %w", repo, err)
	}
	refspecs := buildFetchRefspecs(workBranches)
	out, err := f.upstream.Fetch(ctx, host, f.mirrorDir(repo), upstreamURL, refspecs)
	if err != nil {
		return FetchResult{}, fmt.Errorf("mirror-fetching repo %s: %w", repo, err)
	}
	refs, err := parsePorcelainFetch(out)
	if err != nil {
		return FetchResult{}, fmt.Errorf("parsing mirror-fetch output for repo %s: %w", repo, err)
	}
	return FetchResult{Refs: refs}, nil
}

// mirrorDir derives repo's bare-mirror path. Delegates to
// internal/mirrorpath.Dir, the single source of cmd/server's own
// mirrorPath convention (docs/server-spec.md: "bare mirrors under
// <dir>/mirrors/<group>/<repo_name>.git") -- RepoID is repos.name, already
// the "<group>/<repo_name>" string that convention joins. Kept as a thin
// wrapper (rather than calling mirrorpath.Dir directly at the one call
// site above) so this package's own tests keep exercising it by its
// existing name.
func (f *MirrorFetcher) mirrorDir(repo RepoID) string {
	return mirrorpath.Dir(f.dataDir, string(repo))
}

// buildFetchRefspecs returns the two positive refspecs (every upstream
// branch, every upstream tag), the structural exclusion of Loam's whole
// reserved ref namespace, and then one negative exclusion per name in
// workBranches (docs/git-spec.md -> Ref Policy). Excluding a ref this way
// means the fetch never considers it for this invocation at all -- not
// "fetch then restore" -- so it survives both an upstream force-push of a
// same-named branch and an upstream deletion, since neither ever reaches
// this ref. Nothing outside refs/heads/* and refs/tags/* is fetched at
// all: docs/sync-spec.md's Mirror Sync step 1 says "fetch upstream
// branches and tags", not every upstream ref. That is narrower than this
// function used to be (loam-5f3): it previously paired a single
// "+refs/*:refs/*" wildcard -- byte-identical to what `git clone --mirror`
// configures -- with the same two negative mechanisms below, which also
// pulled in refs/pull/*, refs/notes/*, refs/replace/*, and every other
// upstream namespace. Nothing in loam reads any of those; they only cost
// a 5000+-ref advertisement on every agent clone over /git/*, unbounded
// mirror growth from PR refs pinning objects against gc, and -- the sharp
// one -- refs/replace/* silently altering object visibility, since git
// applies replace refs transparently. Re-widening is cheap if loam ever
// needs to read an upstream PR ref directly rather than through the forge
// REST API.
//
// TWO MECHANISMS, DELIBERATELY. The enumerated per-branch exclusions are
// the SEMANTIC rule -- they say, ref by ref, which refs Loam owns, and
// they are what docs/sync-spec.md's DESIGN mandates. refnames.
// ReservedExclusionRefspec is a STRUCTURAL backstop, not a replacement,
// and it exists because the enumerated list cannot be complete: it is
// resolved from work_branches immediately before this call, but the window
// it must cover is THE ENTIRE DURATION OF THE FETCH, since argv is fixed
// before the network operation begins (seconds to minutes on a large
// repo). A work branch created inside that window is absent from the
// enumerated list, and its brand-new ref -- purely local, never upstream
// -- is therefore a PRUNE candidate, which needs no colliding upstream
// name at all. Verified against real git 2.50.1, and unrecoverable if it
// fires: work_branches carries no SHA column and a bare mirror has no
// reflog, so the row survives pointing at a ref that no longer exists
// (loam-cmq). No amount of re-listing closes that window; a namespace
// glob does, for every work-branch ref that will ever exist. Both
// mechanisms live entirely under refs/heads/ (ReservedNamespace is
// "refs/heads/loam-reserved/"), so narrowing the positive side to
// refs/heads/*+refs/tags/* changes neither: they still land inside the
// branchesRefspec destination exactly as before.
func buildFetchRefspecs(workBranches []string) []string {
	refspecs := make([]string, 0, len(workBranches)+3)
	refspecs = append(refspecs, branchesRefspec, tagsRefspec, refnames.ReservedExclusionRefspec)
	for _, name := range workBranches {
		refspecs = append(refspecs, "^"+refnames.WorkBranch(name))
	}
	return refspecs
}

// porcelainFlags is the fixed set of single-character status flags
// git-fetch(1) OUTPUT documents for --porcelain: ' ' (fast-forward), '+'
// (forced), '-' (pruned), 't' (tag update), '*' (new ref), '!' (rejected),
// and '=' (up to date, --verbose only). A line whose first byte is not one
// of these is not a porcelain ref line at all.
var porcelainFlags = map[byte]bool{' ': true, '+': true, '-': true, 't': true, '*': true, '!': true, '=': true}

// zeroOID reports whether s is a git all-zero object id -- the
// deleted/absent sentinel in `git fetch --porcelain` output -- regardless
// of the hash algorithm's digest length (40 hex chars for SHA-1, 64 for
// SHA-256).
func zeroOID(s string) bool {
	return s != "" && strings.Count(s, "0") == len(s)
}

// isHexOID reports whether s is a plausible git object id: lowercase hex,
// 40 chars (SHA-1) or 64 chars (SHA-256). This also accepts the all-zero
// sentinel, since "0"*40/"0"*64 is valid hex.
func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// parsePorcelainFetchLine parses one line of `git fetch --porcelain`
// output (git-fetch(1) OUTPUT: "<flag> <old-object-id> <new-object-id>
// <local-reference>") into a RefUpdate, reporting ok=false when line does
// not conform to that exact shape: flag is one of porcelainFlags, both
// object-id fields are plausible hex-or-zero, and the ref is rooted at
// "refs/". The flag character itself is not retained in the result -- once
// Fetch has already returned a nil error, every flag this function accepts
// denotes a successful update, and pruned/created rows are already
// distinguishable from the SHA fields alone (an all-zero old id means a
// new ref, an all-zero new id means a prune) without needing the flag too.
func parsePorcelainFetchLine(line string) (RefUpdate, bool) {
	if len(line) < 2 || line[1] != ' ' || !porcelainFlags[line[0]] {
		return RefUpdate{}, false
	}
	fields := strings.Fields(line[1:])
	if len(fields) != 3 {
		return RefUpdate{}, false
	}
	oldSHA, newSHA, ref := fields[0], fields[1], fields[2]
	if !isHexOID(oldSHA) || !isHexOID(newSHA) || !strings.HasPrefix(ref, "refs/") {
		return RefUpdate{}, false
	}
	if zeroOID(oldSHA) {
		oldSHA = ""
	}
	if zeroOID(newSHA) {
		newSHA = ""
	}
	return RefUpdate{Ref: ref, OldSHA: oldSHA, NewSHA: newSHA}, true
}

// parsePorcelainFetch turns `git fetch --porcelain` output into
// RefUpdates, one per conforming line (see parsePorcelainFetchLine). Lines
// that do not conform are silently skipped rather than fabricated into a
// RefUpdate or treated as a fatal parse error: Transport.run returns
// cmd.CombinedOutput() (internal/gittransport/transport.go), so stdout and
// stderr are interleaved in out, and a benign git warning on stderr (e.g.
// "warning: redirecting to https://.../foo.git/" on a redirected upstream
// URL, or fetch's own "From <url>" summary line) is a real, common shape
// here -- neither a ref update to report nor a reason to fail the whole
// sync cycle into sync_state=error.
func parsePorcelainFetch(out []byte) ([]RefUpdate, error) {
	var refs []RefUpdate
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		ref, ok := parsePorcelainFetchLine(line)
		if !ok {
			continue
		}
		refs = append(refs, ref)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning fetch --porcelain output: %w", err)
	}
	return refs, nil
}
