// Command loam is the agent-facing CLI described in docs/cli-spec.md. It
// has no global or persistent flags: all configuration comes from LOAM_*
// environment variables, and every command reads only its own flags.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/bobcob7/loam/internal/cli"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	deps := cli.NewPlaceholderDeps(logger, os.Stdout)
	router := cli.NewRouter(deps)
	os.Exit(cli.Run(context.Background(), router, os.Args[1:]))
}
