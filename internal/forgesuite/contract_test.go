// Package forgesuite is the Provider contract suite (loam-li0.9,
// docs/testing-spec.md Layer 2 "Provider contract"): ONE set of assertions,
// executed unchanged against BOTH forge.Provider implementations —
// internal/fakeforge's Client (fake_test.go, runs in the ordinary
// `go test ./...` gate) and a real Forgejo in a container
// (forgejo_integration_test.go, `//go:build integration` plus an explicit
// LOAM_TEST_FORGEJO=1 opt-in, per testing-spec's CI Stages table putting
// the real-Forgejo leg in the nightly stage). This suite is what licenses
// the acceptance layer to trust the fake; a fake that quietly disagrees
// with Forgejo about an error class, a not-found shape, or pagination is
// exactly what it exists to catch.
//
// It follows internal/storesuite's shape (a dedicated Layer 2 suite
// package whose files are all _test.go, so `go build ./...` and
// `go vet ./...` see an empty directory when the relevant tag is absent)
// and is deliberately split by build tag, not by assertion: contract_test.go
// and recorder_test.go carry EVERY assertion, and the two leg files carry
// only a Harness — the per-implementation plumbing for "seed a repo",
// "hand me a token of kind K", "merge this PR forge-side". Adding an
// assertion to one leg and not the other is not possible without moving
// it out of this file, which is the whole point.
//
// # How the suite refuses to skip a Provider method
//
// Two independent guards, neither of which is a comment:
//
//  1. COMPILE TIME. Every case reaches its Provider through
//     recordingProvider (recorder_test.go), which implements forge.Provider
//     by hand — one explicit method per interface method, no embedded
//     interface. `var _ forge.Provider = (*recordingProvider)(nil)` then
//     means an eighth Provider method fails THIS package's build until
//     someone writes the wrapper for it. An embedded-interface decorator
//     would have satisfied the assertion silently; that is precisely why
//     this one does not embed. Same technique, same reason, as
//     internal/mirrorsync/production_assertions.go.
//  2. RUN TIME. recordingProvider records the name of every method
//     actually invoked, and Run's cleanup compares that set against
//     reflect's view of forge.Provider's real method set — the true
//     population, discovered from the type itself, never a hand-kept list
//     (the same principle as forge.AllSentinels()'s go/ast discovery guard
//     in internal/forge/errors_sentinels_test.go). Writing the wrapper but
//     no case that calls it fails here.
//
// # Deliberate exclusions
//
// One behaviour is knowingly NOT asserted, because the two implementations
// genuinely differ and the divergence is documented rather than papered
// over:
//
//   - CreatePR with a nonexistent HEAD branch. Forgejo 9.0.3 answers HTTP
//     500 with a leaked git error ("exit status 128: ... fatal: bad
//     revision 'refs/heads/main...wb-nope'" — re-confirmed live while
//     writing this suite), so it produces no forge sentinel at all; the
//     fake answers 404 errBranchNotFound, also with no forge sentinel.
//     Mimicking would mean inventing a git error string in the double.
//     internal/forge/provider_test.go and internal/fakeforge/errors.go
//     already pin both halves; see loam-9qu.
//
// CheckRepo against a URL whose host differs from the Provider's bound host
// USED to be excluded here for the same reason (loam-6n3): Forgejo.CheckRepo
// refuses outright (forgejo_git.go's bound-host guard) while
// fakeforge.Client.CheckRepo had no such guard and would happily probe any
// URL. loam-6n3 added the guard to the fake, so this is now asserted below
// as CheckRepo/BoundHostMismatchIsRejected rather than excluded.
//
// A divergence used to be absorbed by the harnesses rather than excluded
// here: the fake modeled a token's git-push scope and its PR-opening scope
// as INDEPENDENT axes (TokenNoGitWrite and TokenNoPRScope, two separate
// registrations), while Forgejo 9.0.3 gates both on the same
// write:repository scope (verified live) — one read:repository token,
// denied on both. loam-2uy collapsed the fake's model to match: there is
// now a single TokenReadOnly, reachable on every leg with one registration,
// and ValidateToken/ReadOnlyTokenIsInsufficientScope below asserts it fails
// ValidateToken exactly as CheckRepo/ReadOnlyTokenIsNoWriteAccess asserts it
// fails the git write probe — the row that would have caught the drift had
// it existed before.
package forgesuite

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/forge"
)

// TokenKind names the three credential shapes the contract needs. A
// Harness maps each onto whatever its forge can actually issue.
//
// TokenNoGitWrite and TokenNoPRScope used to be two separate kinds here,
// on the premise that a forge could grant git push without PR-opening
// scope or vice versa. loam-2uy verified live against Forgejo 9.0.3 that
// it cannot: both are gated on the identical write:repository scope, so
// there is exactly one reachable "authenticates but lacks write access"
// token, not two — TokenReadOnly.
type TokenKind int

const (
	// TokenFull authenticates and carries every scope the Provider needs:
	// git read, git push, and PR creation.
	TokenFull TokenKind = iota
	// TokenReadOnly authenticates and can read over git, but lacks
	// write:repository scope, so it is denied identically on git push
	// (CheckRepo's ErrNoWriteAccess case) and on PR-opening
	// (ValidateToken's ErrInsufficientScope case) — verified live against
	// Forgejo 9.0.3 (loam-2uy).
	TokenReadOnly
	// TokenBogus is a well-formed token string the forge has never issued.
	TokenBogus
)

func (k TokenKind) String() string {
	switch k {
	case TokenFull:
		return "full"
	case TokenReadOnly:
		return "read-only"
	case TokenBogus:
		return "bogus"
	}
	return fmt.Sprintf("TokenKind(%d)", int(k))
}

// Repo is one repository on the forge under test, in the two shapes the
// Provider consumes: Path is the "<owner>/<name>" form CreatePR/GetPRState/
// ClosePR/FindOpenPR take, GitURL is the https clone URL CheckRepo takes.
type Repo struct {
	Path       string
	GitURL     string
	MainBranch string
}

// Harness is the per-implementation plumbing the shared contract runs on.
// It deliberately exposes NO assertion hook: everything a case checks is in
// this file, so both legs check the same things. Everything here is either
// fixture setup or a forge-side event that is not part of forge.Provider.
type Harness interface {
	// Name identifies the implementation in test output.
	Name() string
	// Host is the value ValidateToken should be called with.
	Host(t *testing.T) string
	// Token returns a token of the given kind. Repeated calls for the same
	// kind may return the same token.
	Token(t *testing.T, kind TokenKind) string
	// Provider returns the implementation under test, bound to token for
	// the operations that use a bound credential.
	Provider(t *testing.T, token string) forge.Provider
	// SeedRepo creates a fresh, private, non-empty repository with one
	// commit on its default branch, isolated from every other test's.
	SeedRepo(t *testing.T) Repo
	// MissingRepo names a repository that does not exist on this forge.
	MissingRepo(t *testing.T) Repo
	// MergePR merges an open PR the way the forge itself would — the
	// forge-side event Provider has no operation for, and which ClosePR's
	// already-merged case needs as a precondition.
	MergePR(t *testing.T, repo Repo, prNumber int)
}

// env is what each case gets: the harness, plus the one path to a Provider
// the completeness guard can see.
type env struct {
	h   Harness
	rec *callRecorder
}

// provider returns the implementation under test bound to a token of the
// given kind, wrapped so every call is recorded. Cases MUST obtain their
// Provider this way — a case that reaches around this into h.Provider
// directly would be invisible to the completeness guard.
func (e *env) provider(t *testing.T, kind TokenKind) forge.Provider {
	t.Helper()
	return newRecordingProvider(e.rec, e.h.Provider(t, e.h.Token(t, kind)))
}

// contractCase is one row of the shared contract.
type contractCase struct {
	name string
	run  func(t *testing.T, e *env)
}

// Run executes the whole contract against h. Both legs call exactly this.
func Run(t *testing.T, h Harness) {
	t.Helper()
	rec := &callRecorder{}
	e := &env{h: h, rec: rec}
	t.Cleanup(func() { assertEveryProviderMethodExercised(t, rec) })
	for _, c := range contractCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t, e)
		})
	}
}

// contractCases is the contract. Every case runs unchanged against the fake
// and against real Forgejo.
var contractCases = []contractCase{
	// ---- ValidateToken -------------------------------------------------
	{"ValidateToken/ValidTokenSucceeds", func(t *testing.T, e *env) {
		p := e.provider(t, TokenFull)
		assert.NoError(t, p.ValidateToken(t.Context(), e.h.Host(t), e.h.Token(t, TokenFull)))
	}},
	{"ValidateToken/BogusTokenIsInvalidToken", func(t *testing.T, e *env) {
		p := e.provider(t, TokenFull)
		err := p.ValidateToken(t.Context(), e.h.Host(t), e.h.Token(t, TokenBogus))
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrInvalidToken)
		assert.NotErrorIs(t, err, forge.ErrInsufficientScope, "a token the forge never issued must not read as merely underscoped")
	}},
	{"ValidateToken/EmptyTokenIsInvalidToken", func(t *testing.T, e *env) {
		// Both implementations reach this class by different routes — the
		// real one guards client-side (an empty "Authorization: token "
		// reads as anonymous to Forgejo and would 404 through the scope
		// probe as if it had succeeded), the fake by treating "" as an
		// unregistered token. Same class, different mechanism, which is
		// exactly the agreement most likely to rot unobserved.
		p := e.provider(t, TokenFull)
		err := p.ValidateToken(t.Context(), e.h.Host(t), "")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrInvalidToken)
	}},
	{"ValidateToken/ReadOnlyTokenIsInsufficientScope", func(t *testing.T, e *env) {
		// This is the row loam-2uy added: before it, nothing in the
		// contract asserted that the SAME token CheckRepo/
		// ReadOnlyTokenIsNoWriteAccess proves is denied git push also fails
		// ValidateToken -- the fake modeled those as reachable independently
		// (AddReadOnlyToken kept PR scope), which real Forgejo 9.0.3 cannot
		// produce (verified live: a read:repository token 403s identically
		// on the git-receive-pack advertisement and on POST .../pulls).
		p := e.provider(t, TokenFull)
		err := p.ValidateToken(t.Context(), e.h.Host(t), e.h.Token(t, TokenReadOnly))
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrInsufficientScope)
		assert.NotErrorIs(t, err, forge.ErrInvalidToken, "a scope failure must not also read as an auth failure")
	}},

	// ---- CheckRepo (pure git: ls-remote + receive-pack advertisement,
	// never the REST API) ------------------------------------------------
	{"CheckRepo/ReadableAndWritableSucceeds", func(t *testing.T, e *env) {
		repo := e.h.SeedRepo(t)
		assert.NoError(t, e.provider(t, TokenFull).CheckRepo(t.Context(), repo.GitURL))
	}},
	{"CheckRepo/MissingRepoIsRepoNotFound", func(t *testing.T, e *env) {
		err := e.provider(t, TokenFull).CheckRepo(t.Context(), e.h.MissingRepo(t).GitURL)
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrRepoNotFound)
		assert.NotErrorIs(t, err, forge.ErrNoWriteAccess, "a repo that isn't there must not read as a permissions problem")
	}},
	{"CheckRepo/ReadOnlyTokenIsNoWriteAccess", func(t *testing.T, e *env) {
		repo := e.h.SeedRepo(t)
		err := e.provider(t, TokenReadOnly).CheckRepo(t.Context(), repo.GitURL)
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrNoWriteAccess)
		assert.NotErrorIs(t, err, forge.ErrRepoNotFound, "the read probe passed, so this is not a missing repo")
	}},
	{"CheckRepo/BoundHostMismatchIsRejected", func(t *testing.T, e *env) {
		// loam-6n3: a Provider is bound to one host's credential, so a
		// CheckRepo call against a URL on a DIFFERENT host must be refused
		// outright, before any network probe — a token for one forge must
		// never be sent to another. This is asserted as its own class,
		// distinct from both "repo not found" (the repo may well exist on
		// that foreign host) and "no write access" (the probe never runs).
		// The foreign host uses the reserved .invalid TLD (RFC 2606) so DNS
		// can never accidentally resolve it, making this deterministic
		// regardless of what actually answers the bound host.
		repo := e.h.SeedRepo(t)
		foreignURL := withForeignHost(t, repo.GitURL)
		err := e.provider(t, TokenFull).CheckRepo(t.Context(), foreignURL)
		require.Error(t, err)
		assert.NotErrorIs(t, err, forge.ErrRepoNotFound, "a bound-host mismatch must be rejected on its own terms, not folded into repo-not-found")
		assert.NotErrorIs(t, err, forge.ErrNoWriteAccess, "a bound-host mismatch is not a permissions problem")
	}},
	{"CheckRepo/CredentialBearingURLRedactsPassword", func(t *testing.T, e *env) {
		// loam-giq.12: fakeforge.Client.CheckRepo used to interpolate
		// upstreamURL into its errors verbatim, diverging from
		// Forgejo.CheckRepo's redaction (loam-po8e) — an absence this
		// contract never caught because nothing here asserted on
		// credential handling at all. A caller-supplied upstreamURL can
		// legitimately carry embedded HTTP Basic credentials (this is not
		// the Provider's own bound token, which never appears in a URL at
		// all); whatever those credentials are, they must never appear in
		// an error, even though the host they're attached to is safe to
		// name. The bound-host mismatch path is used to force a
		// deterministic error on every leg without depending on what a
		// live network probe would do with credentials it doesn't expect.
		repo := e.h.SeedRepo(t)
		foreignURL := withForeignHost(t, repo.GitURL)
		const username, password = "loam-contract-user", "s3cr3t-p4ssw0rd"
		err := e.provider(t, TokenFull).CheckRepo(t.Context(), withCredentials(t, foreignURL, username, password))
		require.Error(t, err)
		foreignHost, parseErr := url.Parse(foreignURL)
		require.NoError(t, parseErr)
		assert.Contains(t, err.Error(), foreignHost.Host, "the error should still name the host, which is not secret")
		assert.NotContains(t, err.Error(), password, "the embedded password must never appear in the error")
		assert.NotContains(t, err.Error(), username, "the embedded username must never appear in the error")
	}},
	{"CheckRepo/CredentialBearingURLRedactsUsernameOnlySecret", func(t *testing.T, e *env) {
		// The empty-password PAT form ("https://<token>@host/path") is
		// exactly the shape a Forgejo token takes when embedded in a URL —
		// distinct from the username+password case above because a naive
		// string-replace redaction (hunting for a ":" to find where the
		// password starts) silently misses it: there is no ":" for it to
		// find (loam-po8e, loam-giq.12). A correct redaction reconstructs
		// from the parsed URL with User cleared instead.
		repo := e.h.SeedRepo(t)
		foreignURL := withForeignHost(t, repo.GitURL)
		const usernameOnlySecret = "s3cr3t-t0ken-as-username"
		err := e.provider(t, TokenFull).CheckRepo(t.Context(), withUsernameOnlyCredential(t, foreignURL, usernameOnlySecret))
		require.Error(t, err)
		foreignHost, parseErr := url.Parse(foreignURL)
		require.NoError(t, parseErr)
		assert.Contains(t, err.Error(), foreignHost.Host, "the error should still name the host, which is not secret")
		assert.NotContains(t, err.Error(), usernameOnlySecret, "the embedded username-only secret must never appear in the error")
	}},
	{"CheckRepo/BogusTokenFoldsIntoRepoNotFound", func(t *testing.T, e *env) {
		// Both implementations deliberately FOLD "the credential was
		// rejected on the read probe" into ErrRepoNotFound, because from
		// outside a private repo the two are indistinguishable: Forgejo
		// fails ls-remote with "Authentication failed", the fake 401s its
		// info/refs. This row pins the fold on both sides so a later
		// "improvement" on one side alone shows up here.
		repo := e.h.SeedRepo(t)
		err := e.provider(t, TokenBogus).CheckRepo(t.Context(), repo.GitURL)
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrRepoNotFound)
	}},

	// ---- GitCredentials ------------------------------------------------
	{"GitCredentials/ValidTokenIsAnyUsernamePlusTokenPassword", func(t *testing.T, e *env) {
		// NEVER assert the exact username: Provider's godoc says any
		// username works, the real client sends "loam", the fake sends
		// "fakeforge", and pinning either would encode an implementation
		// detail as a contract.
		token := e.h.Token(t, TokenFull)
		username, password, err := e.provider(t, TokenFull).GitCredentials(t.Context(), token)
		require.NoError(t, err)
		assert.NotEmpty(t, username, "git-over-HTTPS basic auth needs a non-empty username")
		assert.Equal(t, token, password, "Forgejo's convention is the token as the password")
	}},
	{"GitCredentials/EmptyTokenIsInvalidToken", func(t *testing.T, e *env) {
		_, _, err := e.provider(t, TokenFull).GitCredentials(t.Context(), "")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrInvalidToken)
	}},
	{"GitCredentials/AuthenticateARealCloneAndPush", func(t *testing.T, e *env) {
		// The bead's own Definition of Done: the credentials must work in a
		// real `git clone` and a real `git push`, not merely satisfy a REST
		// assertion.
		repo := e.h.SeedRepo(t)
		dir := cloneWithProviderCredentials(t, e, TokenFull, repo)
		branch := "wb-clone-push"
		pushSyntheticBranches(t, dir, repo, branch)
		assert.Contains(t, string(runGit(t, dir, "ls-remote", "origin", "refs/heads/"+branch)), "refs/heads/"+branch,
			"the pushed branch must be visible on the forge, proving the push really landed")
	}},
	{"GitCredentials/ReadOnlyTokenCannotPush", func(t *testing.T, e *env) {
		// The other half of CheckRepo/ReadOnlyTokenIsNoWriteAccess: that
		// case asserts the PROBE's verdict, this one asserts the verdict is
		// true — a real push with the same credential is actually refused.
		repo := e.h.SeedRepo(t)
		dir := cloneWithProviderCredentials(t, e, TokenReadOnly, repo)
		makeSyntheticBranch(t, dir, repo, "wb-denied")
		out, err := tryGit(t, dir, "push", "origin", "refs/heads/wb-denied:refs/heads/wb-denied")
		assert.Error(t, err, "a token without push scope must not be able to push: %s", out)
	}},

	// ---- PR lifecycle --------------------------------------------------
	{"PRLifecycle/OpenPollClosePoll", func(t *testing.T, e *env) {
		repo := e.h.SeedRepo(t)
		p := e.provider(t, TokenFull)
		seedBranches(t, e, repo, "wb-lifecycle")
		prURL, prNumber, err := p.CreatePR(t.Context(), repo.Path, "wb-lifecycle", repo.MainBranch, "title", "description")
		require.NoError(t, err)
		assert.NotEmpty(t, prURL, "CreatePR must return a browsable PR URL")
		assert.Positive(t, prNumber, "CreatePR must return the per-repo PR number")
		state, err := p.GetPRState(t.Context(), repo.Path, prNumber)
		require.NoError(t, err)
		assert.Equal(t, "open", state)
		require.NoError(t, p.ClosePR(t.Context(), repo.Path, prNumber))
		state, err = p.GetPRState(t.Context(), repo.Path, prNumber)
		require.NoError(t, err)
		assert.Equal(t, "closed", state)
	}},
	{"PRLifecycle/OpenMergePoll", func(t *testing.T, e *env) {
		repo := e.h.SeedRepo(t)
		p := e.provider(t, TokenFull)
		seedBranches(t, e, repo, "wb-merge")
		_, prNumber, err := p.CreatePR(t.Context(), repo.Path, "wb-merge", repo.MainBranch, "title", "description")
		require.NoError(t, err)
		e.h.MergePR(t, repo, prNumber)
		state, err := p.GetPRState(t.Context(), repo.Path, prNumber)
		require.NoError(t, err)
		assert.Equal(t, "merged", state, `a merged PR must report "merged", not the "closed" the wire carries`)
	}},
	{"ClosePR/AlreadyMergedIsPRAlreadyMerged", func(t *testing.T, e *env) {
		// loam-giq.8: Forgejo answers 412 Precondition Failed here and
		// leaves the state untouched (re-confirmed live against 9.0.3
		// while writing this suite). Before that bead this fell through
		// doPullRequest's generic "unexpected status" branch, and this row
		// would have had to settle for "some opaque error"; it does not
		// any more.
		repo := e.h.SeedRepo(t)
		p := e.provider(t, TokenFull)
		seedBranches(t, e, repo, "wb-merged-close")
		_, prNumber, err := p.CreatePR(t.Context(), repo.Path, "wb-merged-close", repo.MainBranch, "title", "description")
		require.NoError(t, err)
		e.h.MergePR(t, repo, prNumber)
		err = p.ClosePR(t.Context(), repo.Path, prNumber)
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrPRAlreadyMerged)
		assert.NotErrorIs(t, err, forge.ErrRepoNotFound, "the PR resolved fine; it is merged, not missing")
		state, err := p.GetPRState(t.Context(), repo.Path, prNumber)
		require.NoError(t, err)
		assert.Equal(t, "merged", state, "a refused close must leave the state untouched")
	}},
	{"CreatePR/DuplicateIsDuplicatePRAndFindOpenPRAdoptsIt", func(t *testing.T, e *env) {
		// The idempotency/conflict path loam-giq.7 relies on: a second
		// CreatePR for a pair that already has an open PR is a 409, whose
		// message embeds an INTERNAL id that is not the per-repo number —
		// so adoption has to go through FindOpenPR, and this row proves
		// that round trip end to end on both implementations.
		repo := e.h.SeedRepo(t)
		p := e.provider(t, TokenFull)
		seedBranches(t, e, repo, "wb-dup")
		firstURL, firstNumber, err := p.CreatePR(t.Context(), repo.Path, "wb-dup", repo.MainBranch, "title", "description")
		require.NoError(t, err)
		_, _, err = p.CreatePR(t.Context(), repo.Path, "wb-dup", repo.MainBranch, "title again", "description again")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrDuplicatePR)
		adoptedURL, adoptedNumber, found, err := p.FindOpenPR(t.Context(), repo.Path, "wb-dup", repo.MainBranch)
		require.NoError(t, err)
		require.True(t, found, "the 409 said a PR exists, so the lookup must find it")
		assert.Equal(t, firstNumber, adoptedNumber)
		assert.Equal(t, firstURL, adoptedURL)
	}},
	{"CreatePR/MissingRepoIsRepoNotFound", func(t *testing.T, e *env) {
		_, _, err := e.provider(t, TokenFull).CreatePR(t.Context(), e.h.MissingRepo(t).Path, "wb-x", "main", "title", "description")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrRepoNotFound)
	}},
	{"CreatePR/MissingTargetBranchIsRepoNotFound", func(t *testing.T, e *env) {
		// Forgejo 9.0.3 answers 404 {"message":"BaseNotExist"} — the same
		// generic class as a missing repo, which is why ErrRepoNotFound is
		// the honest sentinel here and the fake's errTargetBranchNotFound
		// wraps it. (Contrast the missing-HEAD-branch case, excluded from
		// this suite; see the package doc.)
		repo := e.h.SeedRepo(t)
		p := e.provider(t, TokenFull)
		seedBranches(t, e, repo, "wb-no-base")
		_, _, err := p.CreatePR(t.Context(), repo.Path, "wb-no-base", "branch-that-does-not-exist", "title", "description")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrRepoNotFound)
	}},
	{"GetPRState/UnknownNumberIsRepoNotFound", func(t *testing.T, e *env) {
		repo := e.h.SeedRepo(t)
		_, err := e.provider(t, TokenFull).GetPRState(t.Context(), repo.Path, 4242)
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrRepoNotFound)
	}},
	{"ClosePR/UnknownNumberIsRepoNotFound", func(t *testing.T, e *env) {
		repo := e.h.SeedRepo(t)
		err := e.provider(t, TokenFull).ClosePR(t.Context(), repo.Path, 4242)
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrRepoNotFound)
		assert.NotErrorIs(t, err, forge.ErrPRAlreadyMerged, "a PR that cannot be resolved is not an already-merged PR")
	}},

	// ---- FindOpenPR ----------------------------------------------------
	{"FindOpenPR/NoOpenPRIsNotFoundWithoutError", func(t *testing.T, e *env) {
		repo := e.h.SeedRepo(t)
		p := e.provider(t, TokenFull)
		prURL, prNumber, found, err := p.FindOpenPR(t.Context(), repo.Path, "wb-absent", repo.MainBranch)
		require.NoError(t, err, "not finding a PR is not an error")
		assert.False(t, found)
		assert.Zero(t, prNumber)
		assert.Empty(t, prURL)
	}},
	{"FindOpenPR/OtherBranchPairIsNotFound", func(t *testing.T, e *env) {
		repo := e.h.SeedRepo(t)
		p := e.provider(t, TokenFull)
		seedBranches(t, e, repo, "wb-pair-a", "wb-pair-b")
		_, _, err := p.CreatePR(t.Context(), repo.Path, "wb-pair-a", repo.MainBranch, "title", "description")
		require.NoError(t, err)
		_, _, found, err := p.FindOpenPR(t.Context(), repo.Path, "wb-pair-b", repo.MainBranch)
		require.NoError(t, err)
		assert.False(t, found, "an open PR on a DIFFERENT head branch must not be returned")
	}},
	{"FindOpenPR/ClosedPRIsNotFound", func(t *testing.T, e *env) {
		repo := e.h.SeedRepo(t)
		p := e.provider(t, TokenFull)
		seedBranches(t, e, repo, "wb-closed")
		_, prNumber, err := p.CreatePR(t.Context(), repo.Path, "wb-closed", repo.MainBranch, "title", "description")
		require.NoError(t, err)
		require.NoError(t, p.ClosePR(t.Context(), repo.Path, prNumber))
		_, _, found, err := p.FindOpenPR(t.Context(), repo.Path, "wb-closed", repo.MainBranch)
		require.NoError(t, err)
		assert.False(t, found, "FindOpenPR is open-only; a closed PR must not be adopted")
	}},
	{"FindOpenPR/MissingRepoIsRepoNotFound", func(t *testing.T, e *env) {
		_, _, _, err := e.provider(t, TokenFull).FindOpenPR(t.Context(), e.h.MissingRepo(t).Path, "wb-x", "main")
		require.Error(t, err)
		assert.ErrorIs(t, err, forge.ErrRepoNotFound)
	}},
	{"FindOpenPR/PagesPastTheFirstPage", func(t *testing.T, e *env) {
		// Forgejo's list-pulls endpoint takes no head/base filter, so the
		// real client pages through EVERY open PR and filters client-side,
		// 50 at a time. With more than one page open, an implementation
		// that reads only the first page finds roughly half of these and
		// misses the rest. Asserting every branch is findable (not just
		// one) makes the row independent of the order the forge lists them
		// in — a single-branch assertion could sit on page 1 by luck and
		// pass against a non-paging implementation.
		repo := e.h.SeedRepo(t)
		p := e.provider(t, TokenFull)
		branches := make([]string, openPRsSpanningTwoPages)
		for i := range branches {
			branches[i] = fmt.Sprintf("wb-page-%02d", i)
		}
		seedBranches(t, e, repo, branches...)
		want := make(map[string]int, len(branches))
		for _, branch := range branches {
			_, prNumber, err := p.CreatePR(t.Context(), repo.Path, branch, repo.MainBranch, "title "+branch, "description")
			require.NoError(t, err)
			want[branch] = prNumber
		}
		for _, branch := range branches {
			_, prNumber, found, err := p.FindOpenPR(t.Context(), repo.Path, branch, repo.MainBranch)
			require.NoError(t, err)
			require.True(t, found, "open PR for %s was not found with %d open PRs on the repo — an implementation that stops after one page fails exactly here", branch, len(branches))
			assert.Equal(t, want[branch], prNumber, "found the wrong PR for %s", branch)
		}
	}},

	// ---- authorization on every bound-credential operation -------------
	{"BoundOperations/BogusTokenIsInvalidToken", func(t *testing.T, e *env) {
		repo := e.h.SeedRepo(t)
		p := e.provider(t, TokenBogus)
		tests := []struct {
			name string
			call func() error
		}{
			{"CreatePR", func() error {
				_, _, err := p.CreatePR(t.Context(), repo.Path, "wb-x", repo.MainBranch, "title", "description")
				return err
			}},
			{"GetPRState", func() error {
				_, err := p.GetPRState(t.Context(), repo.Path, 1)
				return err
			}},
			{"ClosePR", func() error { return p.ClosePR(t.Context(), repo.Path, 1) }},
			{"FindOpenPR", func() error {
				_, _, _, err := p.FindOpenPR(t.Context(), repo.Path, "wb-x", repo.MainBranch)
				return err
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := tt.call()
				require.Error(t, err)
				assert.ErrorIs(t, err, forge.ErrInvalidToken)
			})
		}
	}},
}

// openPRsSpanningTwoPages is one more than the real client's page size
// (forge.listOpenPRsPageSize, 50), the smallest count that forces
// FindOpenPR onto a second page.
const openPRsSpanningTwoPages = 51

// seedBranches creates branches on repo by pushing them with real git,
// authenticating with credentials the Provider under test hands back — so
// even the fixture path goes through GitCredentials on both legs rather
// than reaching around it.
func seedBranches(t *testing.T, e *env, repo Repo, branches ...string) {
	t.Helper()
	dir := cloneWithProviderCredentials(t, e, TokenFull, repo)
	pushSyntheticBranches(t, dir, repo, branches...)
}

// cloneWithProviderCredentials clones repo into a temp dir using the
// username/password GitCredentials returns for a token of the given kind.
func cloneWithProviderCredentials(t *testing.T, e *env, kind TokenKind, repo Repo) string {
	t.Helper()
	token := e.h.Token(t, kind)
	username, password, err := e.provider(t, kind).GitCredentials(t.Context(), token)
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", "--quiet", withCredentials(t, repo.GitURL, username, password), dir)
	return dir
}

// makeSyntheticBranch creates branch locally in the clone at dir, one
// commit ahead of the repo's default branch, without touching a working
// tree: same tree, distinct commit message, so every branch gets a
// distinct SHA for a fraction of the cost of 51 checkout/commit cycles.
func makeSyntheticBranch(t *testing.T, dir string, repo Repo, branch string) {
	t.Helper()
	base := strings.TrimSpace(string(runGit(t, dir, "rev-parse", "refs/remotes/origin/"+repo.MainBranch)))
	tree := strings.TrimSpace(string(runGit(t, dir, "rev-parse", "refs/remotes/origin/"+repo.MainBranch+"^{tree}")))
	commit := strings.TrimSpace(string(runGit(t, dir, "commit-tree", tree, "-p", base, "-m", "loam contract suite: "+branch)))
	runGit(t, dir, "update-ref", "refs/heads/"+branch, commit)
}

// pushSyntheticBranches creates every named branch and pushes them all in
// one invocation.
func pushSyntheticBranches(t *testing.T, dir string, repo Repo, branches ...string) {
	t.Helper()
	args := []string{"push", "--quiet", "origin"}
	for _, branch := range branches {
		makeSyntheticBranch(t, dir, repo, branch)
		args = append(args, "refs/heads/"+branch+":refs/heads/"+branch)
	}
	runGit(t, dir, args...)
}

// withForeignHost returns gitURL with its host replaced by one that is
// guaranteed to differ from whatever host the Provider under test is bound
// to, using the reserved .invalid TLD (RFC 2606) so DNS resolution of the
// foreign host can never accidentally succeed — the point of this helper is
// to prove the bound-host guard fires BEFORE any network probe, not to
// exercise what happens when the foreign host actually answers.
func withForeignHost(t *testing.T, gitURL string) string {
	t.Helper()
	u, err := url.Parse(gitURL)
	require.NoError(t, err)
	u.Host = "bound-to-a-different-forge.invalid"
	return u.String()
}

// withCredentials returns rawURL with username/password embedded as HTTP
// Basic credentials, the form a real git invocation uses to authenticate
// against smart HTTP without a terminal prompt.
func withCredentials(t *testing.T, rawURL, username, password string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	u.User = url.UserPassword(username, password)
	return u.String()
}

// withUsernameOnlyCredential returns rawURL with ONLY a username embedded
// (url.User, never url.UserPassword) — the empty-password PAT form
// "https://<token>@host/path" a Forgejo token takes, distinct from
// withCredentials' username:password form: url.UserPassword(u, "") still
// renders a ":" before the (empty) password, which is exactly the
// character a naive password-only redaction would hunt for and find here
// by accident. url.User(u) renders no ":" at all, so this is the shape
// that actually exercises the naive-redaction failure mode loam-po8e and
// loam-giq.12 fixed.
func withUsernameOnlyCredential(t *testing.T, rawURL, username string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	u.User = url.User(username)
	return u.String()
}

// gitEnv builds the environment for every git invocation this suite makes,
// isolated from every credential store and config file the developer's or
// CI machine happens to have, and with an explicit committer identity.
//
// Both halves are load-bearing. macOS's Command Line Tools ship a SYSTEM
// gitconfig setting credential.helper=osxkeychain, and osxkeychain keys
// entries by protocol+host while IGNORING the port — so a credential
// stored for 127.0.0.1 by any earlier run would be handed to
// GitCredentials/ReadOnlyTokenCannotPush, whose whole point is that the
// credential it supplies must be refused; the guard would silently invert
// (this is not hypothetical: internal/fakeforge/testhelpers_test.go
// records the same inversion happening there). And git auto-detects a
// user@hostname identity on a laptop but FAILS outright on CI without one,
// so commit-tree needs the identity spelled out.
func gitEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	return append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_AUTHOR_NAME=loam-contract-suite",
		"GIT_AUTHOR_EMAIL=contract@example.invalid",
		"GIT_COMMITTER_NAME=loam-contract-suite",
		"GIT_COMMITTER_EMAIL=contract@example.invalid",
	)
}

// runGit runs a git command, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, err := tryGit(t, dir, args...)
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return out
}

// tryGit runs a git command and returns its error, for the cases that
// expect it to fail. credential.helper is cleared with an empty value,
// which git treats as resetting the helper list rather than adding one.
func tryGit(t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "credential.helper="}, args...)...)
	cmd.Dir = dir
	cmd.Env = gitEnv(t)
	return cmd.CombinedOutput()
}
