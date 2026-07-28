package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

// This file holds every byte demo:m5 writes into a git tree -- the work
// branch's own new file, the two files whose overlapping edits produce the
// demo's two conflicts, and the catch-up resolutions -- plus the search
// query asserted against the post-merge index.
//
// They live in Go rather than in the Taskfile for the reason
// cmd/demoenv/fixture.go's header gives and one more that is specific to
// this demo. The token content is load-bearing (see m5SearchQuery), and
// m5fixture_test.go proves the ranking property against the real fixture
// tree on disk -- a proof that is only possible if the bytes are
// addressable from Go. And two of these blobs are posted as JSON string
// fields to the fake forge's control API; a shell heredoc inside a JSON
// body would have to be escaped by hand, which is exactly how a fixture
// silently stops being the thing the test proved.
//
// fixture-polyglot itself is NOT modified, for the reason fixture.go
// states: its files back the pinned ingest goldens. Everything here
// arrives the way real code does -- as commits, pushed or advanced.

const (
	// sessionFilePath is the file the demo's work branch ADDS. It is a new
	// path rather than an edit so its symbol cannot be confused with
	// anything the fixture already defines, and so the post-merge graph
	// query is a claim about a file that could only reach the indexed
	// branch by the pull request merging.
	sessionFilePath = "pkg/session/session.go"

	// sessionSymbol is the symbol whose presence in the graph, at the
	// merge commit, is demo:m5's proof that the loop closed: it exists on
	// the work branch and nowhere upstream until the PR merges.
	sessionSymbol = "StartSession"

	// readmePath and changelogPath are the two conflict surfaces. They are
	// DIFFERENT files on purpose. The demo runs two work branches through
	// one conflicting target advance -- a reviewed one (demoted to draft,
	// conflict 'reset') and a draft one (merely 'flagged') -- and each has
	// to be able to catch up without inheriting the other's resolution. A
	// single shared conflict file would make the second branch's catch-up
	// depend on what the first branch resolved, so the two halves of the
	// reset-versus-flagged distinction could no longer be read
	// independently.
	readmePath    = "README.md"
	changelogPath = "CHANGELOG.md"

	// m5SearchQuery is the query the post-merge search assertion ranks.
	// Every one of its tokens appears verbatim in the contiguous doc
	// comment attached to func StartSession and in NO other file of the
	// post-merge corpus -- which includes the two rewritten Markdown files
	// below, not just fixture-polyglot's own tree. internal/testembed
	// scores by exact-token overlap with no stemming, so every other chunk
	// scores exactly zero against this query and the session chunk ranks
	// first by construction rather than by luck. m5fixture_test.go checks
	// both halves of that (token uniqueness, and hash-bucket collision
	// freedom) against the fixture tree on disk, so rewording any comment
	// in this file fails a unit test before the demo ever runs.
	m5SearchQuery = "issues session handle expires"
)

// sessionFileContent is the work branch's new file: the whole substance of
// the proposal demo:m5 accepts, conflicts, catches up, re-accepts, and
// merges.
//
// Its doc comment's wording is a fixture, not documentation. The four
// m5SearchQuery tokens -- issues, session, handle, expires -- must appear
// literally in the contiguous comment block immediately above func
// StartSession, because internal/ingest/chunker extends a Go chunk
// backward through leading comments and embeds only what lands inside the
// chunk. Nothing else in the corpus may contain them.
const sessionFileContent = `// Package session tracks a signed-in principal's server-side state.
package session

import "time"

// StartSession issues a session handle for an already-signed-in principal
// and reports the instant at which that handle expires.
//
// It arrives on the work branch demo:m5 proposes, so the only route by
// which it can reach the indexed branch is the upstream pull request
// merging. Its presence in the graph, at the merge commit, is what closes
// the loop.
func StartSession(principal string, lifetime time.Duration) (string, time.Time) {
	if principal == "" {
		return "", time.Time{}
	}
	return principal + "@" + lifetime.String(), time.Now().Add(lifetime)
}
`

// proposalReadme is the reviewed work branch's edit to README.md. It
// rewrites the heading line, which is the line the upstream advance below
// also rewrites -- that overlap is what makes git's three-way merge report
// a genuine conflict rather than a clean auto-merge.
const proposalReadme = `# fixture-polyglot (with server-side state)

Seed repository for loam's integration and acceptance test fixtures. See
` + "`docs/OVERVIEW.md`" + ` for the symbol graph this repo is designed to produce.

The proposal adds the fixture's first stateful package.
`

// upstreamReadme is what the target branch advances to, upstream, while
// the proposal is out for an admin decision. It rewrites the same heading
// the proposal rewrote, differently.
const upstreamReadme = `# fixture-polyglot (maintained upstream)

Seed repository for loam's integration and acceptance test fixtures. See
` + "`docs/OVERVIEW.md`" + ` for the symbol graph this repo is designed to produce.

Upstream rewrote this file while a proposal was open against it.
`

// caughtUpReadme is the AUTHOR's conflict resolution: it keeps both sides'
// meaning, which is what a real catch-up produces and what makes the
// resulting commit contain the target tip.
const caughtUpReadme = `# fixture-polyglot (maintained upstream, with server-side state)

Seed repository for loam's integration and acceptance test fixtures. See
` + "`docs/OVERVIEW.md`" + ` for the symbol graph this repo is designed to produce.

Upstream rewrote this file while a proposal was open against it.
The proposal adds the fixture's first stateful package.
`

// draftChangelog is the DRAFT work branch's edit to CHANGELOG.md -- the
// branch that is never sent for review, so the same upstream advance
// merely FLAGS it instead of resetting it.
const draftChangelog = `# Changelog

- initial: fixture seeded
- unreleased: draft work in progress, not yet up for review
`

// upstreamChangelog is the target branch's conflicting rewrite of the same
// file.
const upstreamChangelog = `# Changelog

- initial: fixture seeded
- upstream: maintainers rewrote this file directly
`

// caughtUpChangelog is the draft branch author's resolution.
const caughtUpChangelog = `# Changelog

- initial: fixture seeded
- upstream: maintainers rewrote this file directly
- unreleased: draft work in progress, not yet up for review
`

// m5Fixtures maps the -name values `demoenv fixture-file` accepts to their
// content. A named lookup rather than one subcommand per blob keeps the
// Taskfile's calls uniform and keeps an unknown name a loud failure rather
// than an empty file written into a git tree.
var m5Fixtures = map[string]string{
	"session":             sessionFileContent,
	"proposal-readme":     proposalReadme,
	"upstream-readme":     upstreamReadme,
	"caught-up-readme":    caughtUpReadme,
	"draft-changelog":     draftChangelog,
	"upstream-changelog":  upstreamChangelog,
	"caught-up-changelog": caughtUpChangelog,
	// Not a file: the query the post-merge search assertion uses, exposed
	// here so the Taskfile cannot drift from the constant m5fixture_test.go
	// proves the ranking property about.
	"search-query": m5SearchQuery,
}

// runFixtureFile writes one named fixture blob to stdout verbatim -- no
// trailing newline added, no interpretation -- so the Taskfile can redirect
// it straight into a file in a clone, or capture it with `$(...)` for the
// search query.
//
// An unknown name is an error listing the known ones rather than empty
// output: an empty file committed onto a work branch would sail through
// the push and fail several steps later, naming the wrong culprit.
func runFixtureFile(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("fixture-file", flag.ContinueOnError)
	name := fs.String("name", "", "which fixture blob to print")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("fixture-file requires -name (one of: %s)", strings.Join(fixtureNames(), ", "))
	}
	content, ok := m5Fixtures[*name]
	if !ok {
		return fmt.Errorf("unknown fixture %q (known: %s)", *name, strings.Join(fixtureNames(), ", "))
	}
	if _, err := io.WriteString(stdout, content); err != nil {
		return fmt.Errorf("writing fixture %s: %w", *name, err)
	}
	return nil
}

// fixtureNames lists the known -name values, sorted, for error messages.
func fixtureNames() []string {
	names := make([]string, 0, len(m5Fixtures))
	for name := range m5Fixtures {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// errNoConflictSurface guards against a fixture edit that would silently
// remove the demo's whole point: if the proposal's and upstream's versions
// of a conflict file were ever made identical, git would auto-merge them,
// no branch would be flagged, and the catch-up half of the demo would pass
// vacuously.
var errNoConflictSurface = errors.New("demoenv: the proposal and upstream versions of a conflict file are identical, so no conflict could arise")
