package main

// This file holds the two source files demo:m3 adds on top of
// internal/testfixture's fixture-polyglot, and the vocabulary the demo's
// search assertion depends on. They live in Go rather than in the
// Taskfile because their exact token content is load-bearing -- see
// authFileContent's comment -- and shell heredocs embedded in a JSON
// control-API body would make that content both unreadable and
// unformattable.
//
// fixture-polyglot itself is NOT modified. Its files back the pinned
// ingest goldens (internal/ingest/orchestrator/testdata/golden), so
// adding an "auth" file to the fixture on disk would silently re-baseline
// every golden in the repo. Everything below is added as ordinary
// upstream commits by the demo, exactly as a real upstream push would
// add them, which is also what makes them usable as proof that an ingest
// ran: neither symbol can appear in the index unless the commit carrying
// it was fetched and ingested.

const (
	// legacyFilePath and authFilePath are the two files the demo commits
	// upstream. They are separate paths on purpose: the demo's
	// force-push discards the commit carrying legacyFilePath, so the
	// index must lose LegacySignIn at the same moment it gains Login --
	// a single file changing content could not show both halves.
	legacyFilePath = "pkg/legacy/legacy.go"
	authFilePath   = "pkg/auth/auth.go"

	// legacySymbol is the symbol whose ABSENCE after the force-push
	// proves the index followed a rewritten upstream history rather than
	// merely accumulating commits. It deliberately shares no token with
	// searchQuery below, so it can never compete for the search ranking
	// the demo asserts even while it is still indexed.
	legacySymbol = "LegacySignIn"
	// authSymbol is the symbol whose PRESENCE proves the force-pushed
	// commit was fetched, ingested, and drained.
	authSymbol = "Login"

	// searchQuery is the demo's semantic-search query. Every one of its
	// tokens appears verbatim in authFileContent's doc comment and in no
	// other file of the corpus -- see authFileContent for why that is
	// stated as an exact-token property rather than a semantic one.
	searchQuery = "authentication credentials password"
)

// legacyFileContent is committed upstream BEFORE enrollment, so the first
// (full) ingest indexes it and the demo can then watch it disappear.
//
// Its prose avoids "authentication", "credentials", and "password"
// entirely. That is not stylistic: while this file is still indexed, any
// chunk sharing a searchQuery token would compete for first place in the
// ranking the demo asserts, and a demo whose assertion depends on which
// of two plausible chunks wins is a demo that will flake.
const legacyFileContent = `// Package legacy holds the fixture's superseded entry point.
//
// The demo commits this file, lets it be indexed, and then force-pushes a
// rewritten history that never contained it. Its symbol disappearing from
// the graph is how the demo proves the index tracked upstream's rewrite
// instead of merely growing.
package legacy

// LegacySignIn is the superseded entry point. It is removed, not edited,
// by the rewrite the demo performs.
func LegacySignIn(user string) bool {
	return user != ""
}
`

// authFileContent is committed upstream by the force-push, as the sole
// content of the rewritten tip's new commit.
//
// The doc comment's wording is a fixture, not documentation, and must not
// be "improved" without re-checking the demo. internal/testembed's
// projection is exact-token bag-of-words over the pattern [a-z0-9]+
// after case-folding, with no stemming and no synonyms: "authenticates" and
// "authentication" are different tokens, and a cosine score is nonzero
// only where a query token appears verbatim. searchQuery's three tokens --
// authentication, credentials, password -- therefore have to appear
// literally in this chunk, and must appear in no other chunk of the
// corpus, which together make this chunk the only one with a nonzero
// score and its rank-one position a property of the corpus rather than a
// hope about a model.
//
// The chunk boundary matters too: internal/ingest/chunker chunks Go by
// top-level declaration, extended backward through contiguous leading
// comments (strategies.go's chunkSymbols/leadingCommentStart). The
// package clause and its own comment produce no chunk at all, so the doc
// comment below is embedded only because it is attached to func Login --
// which is also why the tokens are placed there and not above the package
// clause.
const authFileContent = `// Package auth is the fixture's entry point for signing a user in.
package auth

import "strings"

// Login performs password authentication for a username against the
// stored credentials, reporting whether the supplied password matched.
//
// It is committed by the demo's upstream force-push and by nothing else,
// so its presence in the graph is proof that the rewritten tip was
// fetched, ingested, and drained before the query ran.
func Login(username, password string) bool {
	if strings.TrimSpace(username) == "" {
		return false
	}
	return password != ""
}
`
