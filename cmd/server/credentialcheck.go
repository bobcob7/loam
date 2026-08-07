package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bobcob7/loam/internal/config"
	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/crypto"
)

// verifyEncryptionKeyAgainstStoredCredentials is run()'s entry point into
// this file: it builds the same crypto.Encryptor + credentialstore.Store
// pair every register* function in main.go builds (deliberately a THIRD,
// independent construction -- see registerCredentialService's doc comment
// on why those two are already separate from each other), then delegates to
// verifyStoredCredentialsDecrypt below.
//
// Unlike registerCredentialService/registerRepoAdminService, which log and
// skip only their own service on an encryptor-build failure, this treats
// that failure as fatal to the whole process. That is deliberate, not an
// oversight: internal/config already validates LOAM_ENCRYPTION_KEY decodes
// to exactly 32 bytes before run() is ever called, so crypto.NewEncryptor
// failing here would mean config's own validation and this package's
// understanding of it have drifted -- a bug worth stopping the process for,
// not a degraded-but-recoverable condition.
func verifyEncryptionKeyAgainstStoredCredentials(ctx context.Context, cfg config.Config, pool *pgxpool.Pool) error {
	enc, err := crypto.NewEncryptor(cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("building the encryptor to verify LOAM_ENCRYPTION_KEY: %w", err)
	}
	credentials := credentialstore.New(pool, enc, cfg.Logger)
	return verifyStoredCredentialsDecrypt(ctx, credentials, credentials, cfg.Logger)
}

// verifyStoredCredentialsDecrypt is loam-0ab's fix: it confirms, at startup,
// that the LOAM_ENCRYPTION_KEY this process just booted with can actually
// decrypt every already-stored credential, and fails startup loudly if it
// cannot.
//
// # The gap this closes
//
// internal/handler/credential.GetCredentialStatus/ListCredentials
// (docs/web-spec.md "CredentialService") never decrypt anything --
// has_token is `token_ciphertext IS NOT NULL` and validated is a stored
// flag, both by deliberate design (see that package's doc comment: its own
// store seam omits GetByHost, credentialstore.Store's only decrypting
// method, so a token can never reach an RPC response even by accident). The
// consequence, found during loam-ofg.15 and tracked as loam-0ab, is that
// booting with a DIFFERENT key than the one that encrypted an existing row
// -- a swapped secret, a typo, a wrong environment -- makes every admin
// health surface report has_token=true, validated=true for that host, while
// every REAL use of the credential (RepoAdminService.EnrollRepo,
// internal/gittransport's fetch/push, internal/mirrorsync's PR creation)
// fails on internal/crypto's decryption error. The admin's only visibility
// into credential health says everything is fine while nothing works.
//
// # Why this is a seam CredentialService itself cannot fix
//
// Detecting the mismatch needs an actual decrypt attempt, and
// internal/handler/credential's package doc names EXACTLY why that must not
// happen inside the handler: adding GetByHost to its store seam would make
// a token readback expressible in a package whose entire design rests on
// that being impossible. This check runs here instead, at the composition
// root, over credentialstore.Store directly (via forgeCredentialLookup, the
// same seam internal/mirrorsync's forgePRTracker already uses to resolve a
// token) -- a path that already carries plaintext by design, unlike the
// CredentialService handler.
//
// # Fail-fast, not a readiness check
//
// This runs once, in run() (main.go), right after connectDatabase succeeds
// and before any other startup step -- not as a per-request /readyz check.
// Three reasons, in order of weight:
//
//  1. This is a single-replica MVP (docs/server-spec.md "Process Model").
//     /readyz exists to let an orchestrator route around ONE bad instance
//     while others keep serving; with no other replica, failing readiness
//     does not protect availability, it just leaves a process running that
//     reports itself broken forever, with no restart able to fix it (the
//     env var is wrong, not the process). A loud, immediate boot failure is
//     more actionable than a live process quietly failing its readiness
//     probe until an operator notices.
//  2. internal/health's own design draws its readiness line at "does this
//     process fail EVERY request", deliberately excluding the embedder and
//     the forge because their outages only degrade a SUBSET of the surface
//     (see that package's doc comment). A wrong encryption key is exactly
//     that kind of subset failure -- it breaks credential-dependent flows,
//     not repo browsing, search, or graph queries -- so folding it into
//     /readyz would be the same cascade shape that package's doc
//     explicitly warns against: a partial dependency failure taking the
//     whole process out of rotation.
//  3. Unlike a transient dependency outage, a wrong key is never going to
//     start working if this instance is merely restarted with the SAME
//     environment -- it is a static misconfiguration, exactly the class of
//     problem docs/server-spec.md's "Fail fast" paragraph already exists
//     for (missing variables, a key of the wrong length, an unreachable
//     database, an unwritable data dir). This check is the same idea
//     applied one layer deeper: the key's LENGTH can be validated from the
//     environment alone (internal/config), but whether it is the RIGHT key
//     can only be answered by asking the database, which is why it lives
//     here, right after the pool connects, rather than in internal/config.
//
// # A failed READ is not a failed DECRYPT, and the difference is expensive
//
// GetByHost does two things -- it reads the row, then decrypts the
// ciphertext in it -- and its failures therefore come in two shapes that
// call for opposite operator actions:
//
//   - the row could not be READ at all: the pool could not hand out a
//     usable connection, the query failed, the table is not visible. This
//     says nothing whatsoever about LOAM_ENCRYPTION_KEY.
//   - the row was read and could not be DECRYPTED. That is the key
//     mismatch this check exists to catch.
//
// Telling them apart matters more than anything else this function does,
// because the two remedies are not comparable in cost. LOAM_ENCRYPTION_KEY
// cannot be rotated in place and is not covered by a database backup
// (docs/compose-quickstart.md -> "Back up LOAM_ENCRYPTION_KEY before you go
// any further"), so an operator told their key is wrong reasonably
// concludes it is LOST -- and the documented recovery from a lost key is
// deleting every credentials row and re-entering every forge token.
// Reporting a database fault as a key mismatch therefore costs an operator
// every stored credential, on the strength of a diagnosis nothing checked.
//
// # Why the two are classified by evidence rather than assumed
//
// Every error used to be collapsed into the key diagnosis, justified by a
// comment claiming connectDatabase had already proven Postgres reachable.
// It had not, and could not: PGXPOOL IS LAZY. db.NewPool's AfterConnect
// hook (pgvector type registration) runs on EVERY connection the pool
// opens, not just the first, and pgxpool fails the whole acquisition when
// it errors. A pool that connected cleanly at startup still fails every
// later acquisition once the pgvector extension is dropped, a backup is
// restored without it, a failover lands on a database that lacks it, or a
// search_path change hides the type. connectDatabase proves connection #1
// worked and nothing at all about connection #2.
//
// So the shape of the failure is established from evidence that is already
// at hand: ListStatuses reads the same rows over the same pool and decrypts
// NOTHING (see credentialLister). Re-running it after a GetByHost failure
// answers the only question that matters here -- if that plain read fails
// too, the database is not serving reads and the key is not implicated; if
// it succeeds, reading demonstrably works and the decrypt is what failed.
// That needs no error-string matching and no access to internal/crypto's
// unexported decryption sentinel (loam-ai4 tracks exporting it).
//
// The probe travels the same lazy pool it is asking about, so it narrows
// the window rather than closing it: a probe served by an already-open
// connection cannot rule the database out. What it removes is the
// ASSUMPTION -- the key is now only named when a non-decrypting read of the
// same rows has just succeeded.
//
// # The two benign exceptions
//
// ErrNotFound and ErrNoToken are skipped rather than fatal. Both mean the
// row this host's status was read from changed underneath us between
// ListStatuses and GetByHost -- deleted outright, or its token cleared --
// which is a lost race with nothing left to verify, not a key problem.
func verifyStoredCredentialsDecrypt(ctx context.Context, lister credentialLister, lookup forgeCredentialLookup, logger *slog.Logger) error {
	statuses, err := lister.ListStatuses(ctx)
	if err != nil {
		return fmt.Errorf("listing stored credentials to verify LOAM_ENCRYPTION_KEY: %w", err)
	}
	for _, status := range statuses {
		if !status.HasToken {
			continue
		}
		_, err := lookup.GetByHost(ctx, status.Host)
		if err == nil {
			continue
		}
		if errors.Is(err, credentialstore.ErrNotFound) || errors.Is(err, credentialstore.ErrNoToken) {
			continue
		}
		if _, probeErr := lister.ListStatuses(ctx); probeErr != nil {
			return fmt.Errorf("reading the stored credential for host %s failed, and so did a plain re-read that decrypts nothing (%w) -- the database is not serving reads, which is NOT evidence about LOAM_ENCRYPTION_KEY: repair the database and restart, and do NOT touch the key: %w", status.Host, probeErr, err)
		}
		return fmt.Errorf("decrypting the stored credential for host %s: the row read back fine, so LOAM_ENCRYPTION_KEY does not match the key that encrypted it (or the row is corrupt): %w", status.Host, err)
	}
	logger.InfoContext(ctx, "verified LOAM_ENCRYPTION_KEY against stored credentials", "hosts_checked", len(statuses))
	return nil
}
