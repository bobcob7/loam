package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/bobcob7/loam/internal/fakeembed"
	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/testfixture"
)

// prunedBranch is the upstream branch the demo deletes to show that a
// mirror fetch prunes what upstream dropped. It is seeded before
// enrollment so the enrolling clone genuinely mirrors it, which is the
// only way its later disappearance can mean anything.
const prunedBranch = "doomed"

// shutdownGrace bounds the two servers' graceful shutdown once the
// process is signalled, so a stuck git-http-backend CGI child cannot keep
// the demo's cleanup waiting indefinitely.
const shutdownGrace = 5 * time.Second

// runUpstream hosts the fake forge and the fake embedding server for the
// life of the demo.
//
// Both are internal/ packages exposing an http.Handler and no main, and
// both must be reachable from a DIFFERENT process -- the real compiled
// bin/server -- so mounting them in-process (as the acceptance harness
// does) is not available here. This subcommand is that missing process.
//
// It also establishes the entire pre-enrollment upstream state in one
// place, so the demo's shell never has to construct git history:
//
//	commit A  fixture-polyglot, verbatim from internal/testfixture
//	commit B  A + pkg/legacy/legacy.go (LegacySignIn)
//	branch    doomed, at A, seeded so its later deletion can be pruned
//
// Enrollment therefore mirrors main at B with LegacySignIn indexed, which
// is the starting position the `advance` subcommand rewrites.
func runUpstream(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("upstream", flag.ContinueOnError)
	forgeAddr := fs.String("forge-addr", "127.0.0.1:18091", "listen address for the fake forge")
	embedAddr := fs.String("embed-addr", "127.0.0.1:18092", "listen address for the fake embedding server")
	repo := fs.String("repo", "fixture-polyglot", "repo name to seed into the fake forge")
	branch := fs.String("branch", "main", "the repo's default/target branch")
	token := fs.String("token", "", "forge token to accept (required)")
	model := fs.String("embed-model", fakeembed.DefaultModel, "the single model the fake embedder serves")
	workDir := fs.String("work-dir", "", "directory to materialize the fixture into (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" {
		return errors.New("-token is required: the fake forge rejects unauthenticated git with 401, exactly as a real forge does")
	}
	if *workDir == "" {
		return errors.New("-work-dir is required")
	}
	forge, err := fakeforge.New(logger)
	if err != nil {
		return fmt.Errorf("creating fake forge: %w", err)
	}
	defer func() { _ = forge.Close() }()
	forge.AddToken(*token)
	if err := seedUpstream(ctx, forge, *workDir, *repo, *branch); err != nil {
		return err
	}
	// Both listeners are opened BEFORE either server starts serving, and
	// their real addresses are printed only after both bind. A caller
	// polling the printed URLs therefore never races a listener that has
	// not been created yet, and a port already in use fails here with a
	// clear bind error instead of surfacing later as an unexplained
	// connection refused during enrollment.
	forgeLn, err := net.Listen("tcp", *forgeAddr)
	if err != nil {
		return fmt.Errorf("binding fake forge on %s: %w", *forgeAddr, err)
	}
	embedLn, err := net.Listen("tcp", *embedAddr)
	if err != nil {
		_ = forgeLn.Close()
		return fmt.Errorf("binding fake embedder on %s: %w", *embedAddr, err)
	}
	forgeURL := "http://" + forgeLn.Addr().String()
	embedURL := "http://" + embedLn.Addr().String()
	// SetBaseURL is what makes GitURL resolvable; fakeforge panics if
	// GitURL is called before it, so it is set the moment the real
	// address is known.
	forge.SetBaseURL(forgeURL)
	forgeSrv := &http.Server{Handler: forge, ReadHeaderTimeout: 10 * time.Second}
	embedSrv := &http.Server{Handler: fakeembed.New(*model, logger), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- forgeSrv.Serve(forgeLn) }()
	go func() { errs <- embedSrv.Serve(embedLn) }()
	fmt.Printf("forge_url=%s\n", forgeURL)
	fmt.Printf("forge_git_url=%s\n", forge.GitURL(*repo))
	fmt.Printf("embed_url=%s\n", embedURL)
	fmt.Printf("upstream ready\n")
	select {
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving: %w", err)
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	_ = forgeSrv.Shutdown(shutdownCtx)
	_ = embedSrv.Shutdown(shutdownCtx)
	return nil
}

// seedUpstream materializes fixture-polyglot, clones it into the fake
// forge under repo, then builds the pre-enrollment history described on
// runUpstream.
//
// The fixture is materialized to disk first and cloned in rather than
// written file by file through the control API: internal/testfixture owns
// fixture-polyglot's exact content (it is the same tree the ingest
// goldens are pinned against), and re-typing any part of it here would
// let the demo drift from the fixture the rest of the suite tests.
func seedUpstream(ctx context.Context, forge *fakeforge.Server, workDir, repo, branch string) error {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("creating fixture work dir %s: %w", workDir, err)
	}
	fixture, err := testfixture.New(ctx, workDir)
	if err != nil {
		return fmt.Errorf("materializing fixture-polyglot: %w", err)
	}
	// The pruned branch is created on the fixture itself, before the
	// clone, so it arrives in the fake forge as an ordinary branch of the
	// repo's own history -- indistinguishable from one a real upstream
	// had -- rather than as something bolted on afterwards.
	if _, err := fixture.Advance(ctx, prunedBranch); err != nil {
		return fmt.Errorf("creating the %q branch on the fixture: %w", prunedBranch, err)
	}
	if err := forge.SeedRepo(ctx, repo, fixture.Dir()); err != nil {
		return fmt.Errorf("seeding %s into the fake forge: %w", repo, err)
	}
	if err := forge.AdvanceBranch(ctx, repo, branch, fakeforge.AdvanceOptions{
		Path:    legacyFilePath,
		Content: []byte(legacyFileContent),
		Message: "add " + legacySymbol + " (superseded by the demo's rewrite)",
	}); err != nil {
		return fmt.Errorf("committing %s upstream: %w", legacyFilePath, err)
	}
	return nil
}
