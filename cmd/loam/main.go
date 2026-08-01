// Command loam is the agent-facing CLI described in docs/cli-spec.md. It
// has no global or persistent flags: all configuration comes from LOAM_*
// environment variables, and every command reads only its own flags.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/bobcob7/loam/internal/cli"
)

func main() {
	args := os.Args[1:]
	// Help must never require any LOAM_* configuration (loam-dc2v,
	// loam-q0ek): TryHelp is resolved from args alone, before
	// NewProductionDeps ever reads an environment variable.
	if text, ok := cli.TryHelp(args); ok {
		fmt.Fprint(os.Stdout, text)
		os.Exit(0)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	deps, err := cli.NewProductionDeps(logger, http.DefaultClient, os.Stdout, os.Stdin, args)
	if err != nil {
		os.Exit(cli.NewErrorMapper().ExitCode(err))
	}
	router := cli.NewRouter(deps)
	os.Exit(cli.Run(context.Background(), router, args))
}
