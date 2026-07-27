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
// reimplementing internal/crypto in shell. Nor can it be produced over
// RPC: loam.admin.v1.CredentialService.SetUpstreamToken exists in the
// proto but cmd/server registers no handler for it today, so the endpoint
// 404s. Going through internal/credentialstore -- the production store,
// over the production encryptor -- is therefore both the only route and
// the honest one: the row this writes is byte-for-byte the row
// SetUpstreamToken would write once that handler lands.
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
