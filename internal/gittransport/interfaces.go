// Package gittransport runs upstream git subprocesses (the initial
// enrollment mirror clone, mirror fetch, proposal-branch push, upstream
// branch delete, pre-enrollment ls-remote) over HTTPS with the forge
// token injected per invocation (docs/sync-spec.md -> "Upstream
// Transport"). The token is decrypted just-in-time on every call from
// the credential store (loam-54o.8) -- never cached in a package-level
// variable -- converted into the forge's git username/password
// convention, and handed to the git subprocess as an HTTP Authorization
// header via environment-based config injection (GIT_CONFIG_COUNT /
// GIT_CONFIG_KEY_0 / GIT_CONFIG_VALUE_0). It never touches argv, the
// mirror's .git/config, or any file git leaves behind, and the git
// subprocess is isolated from whatever credential helper the host
// machine has configured (see gitEnv in transport.go).
package gittransport

import (
	"context"

	"github.com/bobcob7/loam/internal/credentialstore"
)

//go:generate go tool moq -out moq_test.go . credentialSource gitCredentialConverter

// credentialSource resolves a forge host's decrypted token, defined here
// at the consumer per repo convention. *credentialstore.Store satisfies
// it directly in production (loam-54o.8); tests supply a moq mock.
type credentialSource interface {
	GetByHost(ctx context.Context, host string) (credentialstore.Credential, error)
}

// gitCredentialConverter converts a decrypted token into a forge's
// git-over-HTTPS username/password convention (e.g. Forgejo takes the
// token as the password with any username), defined here at the
// consumer. forge.Provider satisfies it structurally -- only
// GitCredentials from that larger interface is used here; tests supply
// a moq mock instead.
type gitCredentialConverter interface {
	GitCredentials(ctx context.Context, token string) (username, password string, err error)
}
