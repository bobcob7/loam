//go:build acceptance

package main

import "fmt"

// This file holds the content every acceptance scenario's upstream repo
// is seeded with (seedUpstreamRepo, acceptance_sync_test.go).
//
// It used to be a single README.md. That was enough while every scenario
// in the suite only ever asserted on REFS -- which commit a branch points
// at, whether a ref was pruned, whether a push was rejected -- because
// none of them looked at what those commits contained. It stops being
// enough the moment a scenario asserts on an INDEX: a repo with no
// parseable declarations produces no symbols, no edges, and only trivial
// chunks, so "the graph resolves Login" cannot be true of it no matter
// how correctly the ingest pipeline ran.
//
// The files below are therefore chosen to give the ingest pipeline the
// three things ingestion.feature and code-intelligence.feature actually
// assert on, and nothing more:
//
//   - a symbol to find (Login) with a KNOWN definition site, so "I get
//     the file and line of its definition" has a specific answer;
//   - a reference to it from a DIFFERENT file (handler.go calls Login),
//     so cross-file resolution has something to resolve;
//   - a symbol that exists now and must be gone later (LegacyLogin),
//     paired with one that does not exist yet and must appear (Logout),
//     so "advancing the target branch refreshes the index" can be proven
//     in both directions rather than only by something new showing up.
//
// This is deliberately NOT internal/testfixture's fixture-polyglot.
// fixture-polyglot backs the pinned ingest goldens
// (internal/ingest/orchestrator/testdata/golden), which means its content
// cannot be extended to carry an auth symbol without re-baselining every
// golden in the repo, and the symbols it does carry (Validate, Summarize,
// is_even/is_odd) are not the ones these feature files are written
// against. Layer 1 asserts on the vocabulary the specs use; Layer 2
// asserts on fixture-polyglot. Keeping them separate is what lets each
// change without disturbing the other.
//
// Every scenario that predates this content is unaffected by it: they
// assert on refs, push policy, and sync state, none of which depend on
// what a commit contains.

const (
	// acceptanceDefinedSymbol is defined in acceptanceAuthFile and
	// referenced from acceptanceHandlerFile.
	acceptanceDefinedSymbol = "Login"
	// acceptanceRemovedSymbol exists on the seeded commit and is removed
	// by acceptanceAdvancedAuthFile.
	acceptanceRemovedSymbol = "LegacyLogin"
	// acceptanceAddedSymbol does not exist on the seeded commit and is
	// added by acceptanceAdvancedAuthFile.
	acceptanceAddedSymbol = "Logout"
	// acceptanceRenamedSymbol is what acceptanceRenamedAuthContent renames
	// acceptanceDefinedSymbol to, in acceptanceAuthFile only --
	// acceptanceHandlerFile's own bytes are never touched by that commit.
	acceptanceRenamedSymbol = "Authenticate"

	acceptanceAuthFile    = "auth.go"
	acceptanceHandlerFile = "handler.go"
	acceptanceDocFile     = "docs/AUTH.md"
	// acceptanceAdminFile is pushed only by loam-kywt's "an ambiguous
	// symbol returns every match" scenario: a SECOND Go definition of
	// acceptanceDefinedSymbol, in a different file of the SAME repo and
	// the SAME language, so an ambiguous match is proven within one
	// language rather than by internal/testfixture's cross-language Go/
	// TypeScript Validate pair (which loam-w5g's edge-resolution fix
	// deliberately keeps UNlinked -- see acceptanceAdminContent's own
	// doc comment).
	acceptanceAdminFile = "admin.go"
	// acceptanceExtraLoginCallers is how many additional callers of
	// acceptanceDefinedSymbol loam-kywt's truncation scenario pushes on
	// top of handler.go's own Handle, so the dependents of Login
	// genuinely exceed a --limit of 5 (1 + 6 = 7 dependents) rather than
	// the query having room to spare -- a truncation scenario that never
	// crosses its own limit would pass vacuously (this bead's own NOTES).
	acceptanceExtraLoginCallers = 6
)

// acceptanceUpstreamFiles returns the initial commit's tree for repo.
func acceptanceUpstreamFiles(repo string) map[string][]byte {
	return map[string][]byte{
		"README.md":           []byte(fmt.Sprintf("# %s\n\nSee %s for how sign-in works.\n", repo, acceptanceDocFile)),
		acceptanceAuthFile:    []byte(acceptanceAuthContent),
		acceptanceHandlerFile: []byte(acceptanceHandlerContent),
		acceptanceDocFile:     []byte(acceptanceDocContent),
	}
}

// acceptanceAuthContent defines Login (the symbol code-intelligence
// scenarios look up) alongside LegacyLogin (the symbol an advance
// removes). Both are top-level declarations, which is what makes them
// chunkable and extractable at all -- internal/ingest/chunker only emits
// a chunk per top-level declaration, and nested functions get none of
// their own.
const acceptanceAuthContent = `// Package app is the acceptance suite's upstream fixture.
package app

import "strings"

// Login performs password authentication for a username against the
// stored credentials, reporting whether the supplied password matched.
func Login(username, password string) bool {
	if strings.TrimSpace(username) == "" {
		return false
	}
	return password != ""
}

// LegacyLogin is the superseded entry point. A scenario that advances
// the target branch removes it, so its disappearance from the graph is
// how "advancing refreshes the index" is proven in the negative
// direction.
func LegacyLogin(username string) bool {
	return strings.TrimSpace(username) != ""
}
`

// acceptanceHandlerContent lives in a SEPARATE file from Login's
// definition on purpose: a reference in the same file would be satisfied
// by intra-file resolution and would prove nothing about the cross-file
// edges the graph queries are asked for.
const acceptanceHandlerContent = `package app

// Handle authenticates a request by calling Login, giving cross-file
// reference resolution something concrete to find.
func Handle(username, password string) string {
	if Login(username, password) {
		return "ok"
	}
	return "denied"
}
`

// acceptanceDocContent gives semantic search a documentation chunk to
// return alongside the code ones, so "I get the most relevant doc and
// code chunks" is answerable rather than vacuously code-only. Its
// headings matter: the markdown strategy emits one chunk per ATX
// heading, so the sections below are the chunk boundaries.
const acceptanceDocContent = `# Authentication

## Signing in

Authentication is handled by Login in auth.go, which checks the supplied
password against the stored credentials for that username.

## Sessions

A successful sign-in is what later requests are authorized against.
`

// acceptanceAdvancedAuthContent is acceptanceAuthContent with
// LegacyLogin removed and Logout added, in one commit. Both halves are
// in the same file so a single fakeforge advance (which writes exactly
// one path) can perform an addition and a removal together, which is
// what ingestion.feature's "adds X and removes Y" step describes.
const acceptanceAdvancedAuthContent = `// Package app is the acceptance suite's upstream fixture.
package app

import "strings"

// Login performs password authentication for a username against the
// stored credentials, reporting whether the supplied password matched.
func Login(username, password string) bool {
	if strings.TrimSpace(username) == "" {
		return false
	}
	return password != ""
}

// Logout ends a signed-in session. It is added by the advance, so
// finding it proves the index was rebuilt from the new tip.
func Logout(username string) bool {
	return strings.TrimSpace(username) != ""
}
`

// acceptanceRenamedAuthContent is acceptanceAuthContent with Login
// renamed to Authenticate -- and nothing else touched: LegacyLogin stays,
// and acceptanceHandlerFile is never part of this commit. That is the
// fixture loam-d2b2's rewrite of "Edges reflect the current code even in
// unchanged files" needs: a rename confined to the DEFINING file, so the
// referencing file (which still says "Login") is provably unchanged.
const acceptanceRenamedAuthContent = `// Package app is the acceptance suite's upstream fixture.
package app

import "strings"

// Authenticate performs password authentication for a username against
// the stored credentials, reporting whether the supplied password
// matched.
func Authenticate(username, password string) bool {
	if strings.TrimSpace(username) == "" {
		return false
	}
	return password != ""
}

// LegacyLogin is the superseded entry point. A scenario that advances
// the target branch removes it, so its disappearance from the graph is
// how "advancing refreshes the index" is proven in the negative
// direction.
func LegacyLogin(username string) bool {
	return strings.TrimSpace(username) != ""
}
`

// acceptanceAdminContent defines a SECOND, independent acceptanceDefinedSymbol
// ("Login") in a different Go file of the same repo -- what "An ambiguous
// symbol returns every match" pushes on top of the Background's own
// auth.go/handler.go. Deliberately a different signature (token, not
// username/password) from acceptanceAuthContent's Login: nothing about
// this fixture depends on the two bodies agreeing, only on there being two
// distinct definitions for a name lookup to find, and this repo's content
// is never `+"`go build`"+`-checked by the ingest pipeline (Tree-sitter parses
// each file independently).
//
// This is deliberately Go, not internal/testfixture's cross-language Go/
// TypeScript Validate pair: loam-w5g narrowed graph_edges resolution to
// stay INTRA-language, so proving ambiguity with a cross-language pair
// would conflate two different properties -- LookupSymbolsByName's name
// lookup (language-agnostic, exercised here) and ResolveGraphEdgeCandidates'
// edge resolution (intra-language since loam-w5g, exercised by "Finding
// what depends on a target" instead). Keeping them on separate fixtures is
// what keeps the two scenarios from contradicting each other.
const acceptanceAdminContent = `// Package app is the acceptance suite's upstream fixture.
package app

// Login authenticates an administrator session by a bearer token instead
// of a username/password pair, so a name lookup for "Login" over this
// repo's Go code genuinely has two independent definitions to return.
func Login(token string) bool {
	return token != ""
}
`
