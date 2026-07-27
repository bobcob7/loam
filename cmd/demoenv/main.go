// Command demoenv is the support tool for `task demo:m3`. It is NOT part
// of the shipped product: `task build:bin` builds exactly the three
// binaries this repo ships (server, loam, loamhook) and deliberately does
// not build this one, because cmd/server's startup contract is that
// loamhook sits beside it and nothing else has to.
//
// It exists because demo:m3 needs three things from real Loam packages
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
//
// Subcommands:
//
//	upstream         host the fake forge + fake embedder, seeded with
//	                 fixture-polyglot, until signalled
//	advance          rewrite the upstream target branch (force-push) and
//	                 delete the branch whose prune the demo asserts
//	seed-credential  write the encrypted forge token EnrollRepo needs
//	check-envelope   assert over a `loam graph`/`loam search` JSON envelope
//	check-jobs       assert over a ListIngestJobs JSON response
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
const usage = `demoenv is the support tool for task demo:m3 (not a shipped binary).

usage: demoenv <subcommand> [flags]

subcommands:
  upstream         host the fake forge + fake embedder, seeded with fixture-polyglot
  advance          force-push the upstream target branch and delete the pruned branch
  seed-credential  write the encrypted forge token EnrollRepo requires
  check-envelope   assert over a loam graph/search JSON envelope read from stdin
  check-jobs       assert over a ListIngestJobs JSON response read from stdin
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
	case "advance":
		err = runAdvance(ctx, os.Args[2:])
	case "seed-credential":
		err = runSeedCredential(ctx, logger, os.Args[2:])
	case "check-envelope":
		err = runCheckEnvelope(os.Args[2:], os.Stdin, os.Stdout)
	case "check-jobs":
		err = runCheckJobs(os.Args[2:], os.Stdin, os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "demoenv: unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "demoenv %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}
