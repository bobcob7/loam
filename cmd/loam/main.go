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

// exitUsage is the CLI's exit code for a usage failure (see
// docs/cli-spec.md -> Exit Codes & Errors), including a missing or
// malformed required LOAM_* configuration variable (see cli-spec ->
// whoami).
const exitUsage = 2

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	deps, err := cli.NewPlaceholderDeps(logger, os.Stdout)
	if err != nil {
		os.Exit(exitUsage)
	}
	router := cli.NewRouter(deps)
	os.Exit(cli.Run(context.Background(), router, os.Args[1:]))
}
