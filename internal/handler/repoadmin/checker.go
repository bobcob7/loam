package repoadmin

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/bobcob7/loam/internal/forge"
)

// ForgeChecker adapts forge.NewForgejo to this package's upstreamChecker
// seam, satisfying it structurally. Unlike gittransport.Transport's
// gitCredentialConverter (a single, host-agnostic *forge.Forgejo built
// once at composition-root time, since GitCredentials' convention is the
// same for every Forgejo host), CheckRepo enforces that upstreamURL's
// host matches the *forge.Forgejo instance's OWN bound host
// (forge/forgejo_git.go's CheckRepo doc comment), so a fresh instance
// bound to host+token must be built per call -- EnrollRepo's only use of
// this adapter, never reused or cached across repos.
type ForgeChecker struct {
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// CheckRepo builds a single-use *forge.Forgejo bound to host+token and
// delegates to its CheckRepo, satisfying upstreamChecker.
func (f ForgeChecker) CheckRepo(ctx context.Context, host, token, upstreamURL string) error {
	return forge.NewForgejo(host, token, f.HTTPClient, f.Logger).CheckRepo(ctx, upstreamURL)
}
