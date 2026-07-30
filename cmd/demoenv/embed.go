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
)

// embedShutdownGrace bounds this server's graceful shutdown once the
// process is signalled, mirroring runUpstream's own shutdownGrace.
const embedShutdownGrace = 5 * time.Second

// runEmbed hosts the fake embedding server alone, for stacks that already
// have a real upstream forge (test:e2e's Postgres/pgvector + seeded real
// Forgejo, deploy/docker-compose.e2e.yml) and only need LOAM_EMBEDDER_URL
// pointed somewhere reachable that speaks Ollama's wire shape.
//
// runUpstream bundles internal/fakeforge and internal/fakeembed together
// because demo:m3's upstream is itself fake; test:e2e's upstream is a real
// Forgejo container, so pulling in a second fake forge here would be
// pointless (and would defeat the point of that stack per
// docker-compose.e2e.yml's own header comment). This subcommand is the
// same "host process for a library http.Handler" idea runUpstream
// documents, scoped to only the half test:e2e actually needs.
func runEmbed(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("embed", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:18092", "listen address for the fake embedding server")
	model := fs.String("model", fakeembed.DefaultModel, "the single model the fake embedder serves")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("binding fake embedder on %s: %w", *addr, err)
	}
	srv := &http.Server{Handler: fakeembed.New(*model, logger), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(ln) }()
	fmt.Fprintf(os.Stdout, "embed_url=http://%s\n", ln.Addr().String())
	fmt.Fprintln(os.Stdout, "embed ready")
	select {
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving: %w", err)
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), embedShutdownGrace)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}
