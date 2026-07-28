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

// runUpstreamM5 hosts demo:m5's fake upstream: one internal/fakeforge
// Server fronted by the Forgejo-REST-shaped pull-request surface in
// forgejoapi.go, plus one internal/fakeembed embedding server, both
// reachable over real sockets by the real compiled bin/server.
//
// It is a SEPARATE subcommand from `upstream` (demo:m3's), not a flag on
// it, for two reasons:
//
//   - `upstream` bakes in demo:m3's whole pre-enrollment history -- the
//     LegacySignIn commit whose later disappearance proves a rewrite was
//     followed, and the `doomed` branch whose pruning proves a delete was
//     followed. Neither is a fixture demo:m5 wants: an extra branch and an
//     extra symbol would only add noise to a demo about pull requests, and
//     the force-push `advance` performs is the opposite of the ordinary,
//     fast-forward target advance this demo needs.
//   - demo:m3 and internal/fakeforge are being changed concurrently
//     (loam-7d2, which adds a Forgejo-shaped ValidateToken surface to the
//     fake and rewires demo:m3's credential seeding). Two demos sharing
//     one host subcommand would make every such change a change to both.
//
// The seeded upstream here is fixture-polyglot verbatim, with nothing
// added: everything demo:m5 observes arrives later, as a real push or a
// real control-API advance.
func runUpstreamM5(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("upstream-m5", flag.ContinueOnError)
	forgeAddr := fs.String("forge-addr", "127.0.0.1:18095", "listen address for the fake forge")
	embedAddr := fs.String("embed-addr", "127.0.0.1:18096", "listen address for the fake embedding server")
	repo := fs.String("repo", "loam-demo/doc-server", "repo name to seed into the fake forge, \"<owner>/<name>\"")
	token := fs.String("token", "", "forge token to accept (required)")
	model := fs.String("embed-model", fakeembed.DefaultModel, "the single model the fake embedder serves")
	workDir := fs.String("work-dir", "", "directory to materialize the fixture into (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" {
		return errors.New("-token is required: the fake forge rejects unauthenticated git with 401, and the Forgejo REST shim rejects an unauthenticated PR call with 401, exactly as a real forge does")
	}
	if *workDir == "" {
		return errors.New("-work-dir is required")
	}
	forgeServer, err := fakeforge.New(logger)
	if err != nil {
		return fmt.Errorf("creating fake forge: %w", err)
	}
	defer func() { _ = forgeServer.Close() }()
	forgeServer.AddToken(*token)
	if err := os.MkdirAll(*workDir, 0o755); err != nil {
		return fmt.Errorf("creating fixture work dir %s: %w", *workDir, err)
	}
	fixture, err := testfixture.New(ctx, *workDir)
	if err != nil {
		return fmt.Errorf("materializing fixture-polyglot: %w", err)
	}
	if err := forgeServer.SeedRepo(ctx, *repo, fixture.Dir()); err != nil {
		return fmt.Errorf("seeding %s into the fake forge: %w", *repo, err)
	}
	// Both listeners bind BEFORE either server serves, and the addresses
	// are printed only once both are up, for the reason runUpstream's own
	// comment gives: a caller polling the printed URLs then never races a
	// listener that does not exist yet, and a port already in use fails
	// here with a clear bind error rather than as an unexplained
	// connection refused several steps later.
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
	forgeServer.SetBaseURL(forgeURL)
	// The shim is constructed only now, because it talks to the fake's own
	// provider REST surface over the loopback address that did not exist
	// until the listener bound.
	api := newForgejoAPI(forgeServer, forgeURL, *token, logger)
	forgeSrv := &http.Server{Handler: api, ReadHeaderTimeout: 10 * time.Second}
	embedSrv := &http.Server{Handler: fakeembed.New(*model, logger), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- forgeSrv.Serve(forgeLn) }()
	go func() { errs <- embedSrv.Serve(embedLn) }()
	fmt.Printf("forge_url=%s\n", forgeURL)
	fmt.Printf("forge_git_url=%s\n", forgeServer.GitURL(*repo))
	fmt.Printf("forge_api_url=%s/api/v1\n", forgeURL)
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

// runConflictingAdvance performs demo:m5's ONE upstream event: an ordinary
// fast-forward advance of the target branch that rewrites both conflict
// files, and therefore no longer merges with either of the demo's two work
// branches.
//
// It is an ORDINARY advance, not demo:m3's force-push, and that is the
// point: this demo's subject is what a conflicting target advance does to
// an open proposal, which is a question about merge-ability, not about
// whether the mirror follows a rewrite. Two commits rather than one --
// README.md for the reviewed branch, CHANGELOG.md for the draft one -- so
// each branch's catch-up resolves only its own file and the
// reset-versus-flagged distinction can be read independently on each.
//
// The tip is read back from UPSTREAM with a real authenticated ls-remote
// (never from the mirror), so the value the demo asserts the index against
// comes from the side of the system that is not under test.
func runConflictingAdvance(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("conflicting-advance", flag.ContinueOnError)
	forgeURL := fs.String("forge-url", "", "base URL of the running fake forge (required)")
	gitURL := fs.String("git-url", "", "authenticated git URL of the repo, used to read back the new tip (required)")
	repo := fs.String("repo", "loam-demo/doc-server", "repo name inside the fake forge")
	branch := fs.String("branch", "main", "target branch to advance")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *forgeURL == "" {
		return errors.New("-forge-url is required")
	}
	if *gitURL == "" {
		return errors.New("-git-url is required")
	}
	if upstreamReadme == proposalReadme || upstreamChangelog == draftChangelog {
		return errNoConflictSurface
	}
	before, err := remoteTip(ctx, *gitURL, *branch)
	if err != nil {
		return err
	}
	advances := []struct {
		path    string
		content string
		message string
	}{
		{readmePath, upstreamReadme, "docs: rewrite the README heading upstream"},
		{changelogPath, upstreamChangelog, "docs: rewrite the changelog upstream"},
	}
	for _, advance := range advances {
		if err := control(ctx, *forgeURL, "/control/advance-branch", map[string]any{
			"repo":    *repo,
			"branch":  *branch,
			"path":    advance.path,
			"content": advance.content,
			"message": advance.message,
		}); err != nil {
			return fmt.Errorf("advancing %s with a conflicting %s: %w", *branch, advance.path, err)
		}
	}
	after, err := remoteTip(ctx, *gitURL, *branch)
	if err != nil {
		return err
	}
	if after == before {
		return fmt.Errorf("the advance left %s at %s: upstream never moved, so no conflict could be detected", *branch, before)
	}
	fmt.Printf("previous_ref=%s\n", before)
	fmt.Printf("new_ref=%s\n", after)
	fmt.Printf("conflict_files=%s,%s\n", readmePath, changelogPath)
	return nil
}
