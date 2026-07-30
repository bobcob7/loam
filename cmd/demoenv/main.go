// Command demoenv is the support tool for `task demo:m3`, `task demo:m4`
// and `task demo:m5`. It is NOT part of the shipped product: `task
// build:bin` builds
// exactly the three binaries this repo ships (server, loam, loamhook) and
// deliberately does not build this one, because cmd/server's startup
// contract is that loamhook sits beside it and nothing else has to.
//
// It exists because those demos need three things from real Loam packages
// that no shipped binary provides, and that a shell script cannot produce
// on its own:
//
//   - a HOST PROCESS for internal/fakeforge and internal/fakeembed. Both
//     are libraries -- an http.Handler each -- with no main of their own.
//     The demo drives the real compiled server, so the fake upstream forge
//     and the fake embedding server have to be reachable over real
//     sockets from another process, not mounted in-process the way the
//     acceptance harness mounts them.
//   - a way to write the encrypted forge CREDENTIAL row EnrollRepo
//     requires. The token is stored AES-encrypted under
//     LOAM_ENCRYPTION_KEY (internal/credentialstore over internal/crypto),
//     so a psql INSERT of the kind demo:m2 uses for repos/work_branches
//     cannot produce it, and CredentialService is not registered in
//     cmd/server today (see registerRepoAdminService's neighbours in
//     cmd/server/main.go) so there is no RPC to call either.
//   - ASSERTIONS over the JSON the loam CLI and the admin RPCs emit. The
//     demo has to exit non-zero when a ranking or an ingested ref is
//     wrong, and doing that with grep against JSON is exactly the kind of
//     approximate check that passes for the wrong reason. jq would solve
//     it but is a host prerequisite this repo has never taken; a Go
//     subcommand keeps the demo's dependency set at "the Go toolchain,
//     git, curl, and a docker CLI", the same set demo:m1 and demo:m2
//     already need.
//   - a FORGEJO-REST-SHAPED pull-request surface in front of the fake
//     forge (forgejoapi.go, added for demo:m5). internal/fakeforge serves
//     its own /provider/* REST shape, which the acceptance suite reaches
//     through a *fakeforge.Client substituted for the whole
//     forge.Provider; the real compiled server instead builds a
//     *forge.Forgejo and calls Forgejo's actual /api/v1/repos/.../pulls
//     endpoints, which the fake does not serve. See forgejoapi.go for why
//     that translator lives here rather than in internal/fakeforge.
//
// Subcommands:
//
//	upstream         host the fake forge + fake embedder, seeded with
//	                 fixture-polyglot, until signalled
//	upstream-m5      the same pair for demo:m5, with a Forgejo-REST-shaped
//	                 pull-request surface in front of the fake forge
//	embed            host the fake embedder alone, for stacks (test:e2e)
//	                 that already have a real upstream forge
//	advance          rewrite the upstream target branch (force-push) and
//	                 delete the branch whose prune the demo asserts
//	conflicting-advance  advance the target branch so it no longer merges
//	                 with either of demo:m5's work branches
//	fixture-file     print one of demo:m5's fixture blobs verbatim
//	seed-credential  write the encrypted forge token EnrollRepo needs
//	check-envelope   assert over a `loam graph`/`loam search` JSON envelope
//	check-jobs       assert over a ListIngestJobs JSON response
//	check-comments   assert over a `loam work comments` document, staged or not
//	check-verdicts   assert over a `loam work verdicts` document
//	check-worklist   assert over a `loam work list` document
//	check-proposals  assert over a ListProposals JSON response
//	check-prs        assert over the forge's own Forgejo pull-request list
//	thread-id        print the id of the thread anchored to a given file
//	field            print one named top-level string field of a JSON object
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// usage lists the subcommands, printed on a missing or unknown one.
const usage = `demoenv is the support tool for task demo:m3, demo:m4 and demo:m5 (not a shipped binary).

usage: demoenv <subcommand> [flags]

subcommands:
  upstream             host the fake forge + fake embedder, seeded with fixture-polyglot
  upstream-m5          the same, with a Forgejo-REST-shaped pull-request surface in front
  embed                host the fake embedder alone, for a stack with a real upstream forge
  advance              force-push the upstream target branch and delete the pruned branch
  conflicting-advance  advance the target branch so open work branches no longer merge
  fixture-file         print one of demo:m5's fixture blobs verbatim
  seed-credential      write the encrypted forge token EnrollRepo requires
  check-envelope       assert over a loam graph/search JSON envelope read from stdin
  check-jobs           assert over a ListIngestJobs JSON response read from stdin
  check-comments       assert over a loam work comments document read from stdin
  check-verdicts       assert over a loam work verdicts document read from stdin
  check-worklist       assert over a loam work list document read from stdin
  check-proposals      assert over a ListProposals JSON response read from stdin
  check-prs            assert over a Forgejo pull-request list read from stdin
  thread-id            print the id of the thread anchored to -file
  field                print one named top-level string field of a JSON object
`

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// Only `upstream` is long-running, so only it takes a signal-cancelled
	// context; every other subcommand is a one-shot that either completes
	// or fails. NotifyContext covers both SIGINT (an operator's Ctrl-C)
	// and SIGTERM (how the demo's own cleanup stops it).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "upstream":
		err = runUpstream(ctx, logger, os.Args[2:])
	case "upstream-m5":
		err = runUpstreamM5(ctx, logger, os.Args[2:])
	case "embed":
		err = runEmbed(ctx, logger, os.Args[2:])
	case "advance":
		err = runAdvance(ctx, os.Args[2:])
	case "conflicting-advance":
		err = runConflictingAdvance(ctx, os.Args[2:])
	case "fixture-file":
		err = runFixtureFile(os.Args[2:], os.Stdout)
	case "seed-credential":
		err = runSeedCredential(ctx, logger, os.Args[2:])
	case "check-envelope":
		err = runCheckEnvelope(os.Args[2:], os.Stdin, os.Stdout)
	case "check-jobs":
		err = runCheckJobs(os.Args[2:], os.Stdin, os.Stdout)
	case "check-comments":
		err = runCheckComments(os.Args[2:], os.Stdin, os.Stdout)
	case "check-verdicts":
		err = runCheckVerdicts(os.Args[2:], os.Stdin, os.Stdout)
	case "check-worklist":
		err = runCheckWorkList(os.Args[2:], os.Stdin, os.Stdout)
	case "check-proposals":
		err = runCheckProposals(os.Args[2:], os.Stdin, os.Stdout)
	case "check-prs":
		err = runCheckPRs(os.Args[2:], os.Stdin, os.Stdout)
	case "thread-id":
		err = runThreadID(os.Args[2:], os.Stdin, os.Stdout)
	case "field":
		err = runField(os.Args[2:], os.Stdin, os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "demoenv: unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "demoenv %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}
