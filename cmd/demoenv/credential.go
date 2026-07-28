package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/crypto"
)

// runSeedCredential writes the encrypted forge token EnrollRepo requires.
//
// EnrollRepo resolves the upstream URL's host to a credential row and
// fails with FailedPrecondition when there is none
// (internal/handler/repoadmin/enroll.go), and the mirror fetcher resolves
// the same row on every sync tick. So the demo cannot enroll anything
// without one.
//
// It cannot be produced the way demo:m2 produces its repos/work_branches
// rows, with a psql INSERT: the token is stored AES-GCM encrypted under
// LOAM_ENCRYPTION_KEY, and reproducing that ciphertext in SQL would mean
// reimplementing internal/crypto in shell.
//
// # Why not the RPC (the reason changed; the answer did not)
//
// This comment used to say loam.admin.v1.CredentialService.SetUpstreamToken
// had no registered handler and 404'd. That stopped being true with
// loam-ofg.15: the service is implemented in internal/handler/credential
// and registered in cmd/server. The RPC is still unusable HERE, for a
// different and narrower reason, filed as loam-4kz:
//
// SetUpstreamToken validates before it writes, through the real
// *forge.Forgejo, and forge's apiBaseURL prepends "https://" to any host
// string that does not already carry a scheme. The host string is not
// free: EnrollRepo resolves a credential by deriveRepoIdentity's u.Host,
// a BARE authority. So for the demo's plaintext-HTTP fake forge the two
// requirements are mutually exclusive -- the bare host EnrollRepo will
// look up ("127.0.0.1:<port>") is addressed over https and fails
// validation with "server gave HTTP response to HTTPS client", while the
// scheme-bearing host that validates fine ("http://127.0.0.1:<port>")
// writes a row under a key EnrollRepo never looks up. Serving the fake
// over TLS instead is not an escape: the server's validator is a plain
// &http.Client{}, there is no LOAM_* CA knob, and Go ignores
// SSL_CERT_FILE on darwin.
//
// internal/fakeforge now DOES answer the Forgejo-shaped scope probe
// ValidateToken issues (internal/fakeforge/forgejoapi.go, loam-7d2), so
// the forge side of that RPC is genuinely exercisable against the fake --
// just not under a bare host:port. When loam-4kz lands, demo:m3's step 5
// collapses into a plain admin RPC like its step 6 and this subcommand
// can go.
//
// Until then, going through internal/credentialstore -- the production
// store, over the production encryptor -- remains both the only route and
// the honest one: the row this writes is byte-for-byte the row
// SetUpstreamToken writes.
func runSeedCredential(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("seed-credential", flag.ContinueOnError)
	databaseURL := fs.String("database-url", "", "Postgres connection URL (required)")
	host := fs.String("host", "", "forge host the token authenticates to, e.g. 127.0.0.1:18091 (required)")
	token := fs.String("token", "", "the forge token (required)")
	encryptionKey := fs.String("encryption-key", "", "base64 LOAM_ENCRYPTION_KEY, the same value the server runs with (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *databaseURL == "":
		return errors.New("-database-url is required")
	case *host == "":
		return errors.New("-host is required")
	case *token == "":
		return errors.New("-token is required")
	case *encryptionKey == "":
		return errors.New("-encryption-key is required")
	}
	// The key is decoded here exactly as internal/config decodes
	// LOAM_ENCRYPTION_KEY, so a key this accepts is a key the server
	// accepts, and a mismatch fails here rather than surfacing later as
	// an undecryptable token during the first fetch.
	key, err := base64.StdEncoding.DecodeString(*encryptionKey)
	if err != nil {
		return fmt.Errorf("decoding -encryption-key as base64: %w", err)
	}
	encryptor, err := crypto.NewEncryptor(key)
	if err != nil {
		return fmt.Errorf("building encryptor: %w", err)
	}
	pool, err := pgxpool.New(ctx, *databaseURL)
	if err != nil {
		return fmt.Errorf("connecting to Postgres: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging Postgres: %w", err)
	}
	if _, err := credentialstore.New(pool, encryptor, logger).UpsertToken(ctx, *host, *token); err != nil {
		return fmt.Errorf("upserting the credential for host %s: %w", *host, err)
	}
	fmt.Printf("credential stored for host %s\n", *host)
	return nil
}
