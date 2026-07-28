package credentialstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bobcob7/loam/internal/db/gen"
)

// CredentialStatus is a host's presence and validation state, backing
// CredentialService.GetCredentialStatus/ListCredentials (docs/web-spec.md
// "CredentialService"): { host, has_token, validated }. HasToken is
// derived from token_ciphertext IS NOT NULL at query time -- never a
// separate stored flag -- and neither field here ever touches the
// ciphertext.
type CredentialStatus struct {
	Host      string
	HasToken  bool
	Validated bool
}

// Credential is a decrypted credentials row: Token is plaintext, ready for
// git credential injection or a forge REST call (docs/sync-spec.md
// "Upstream Transport"). Only GetByHost ever produces one of these --
// every other Store method stays on the CredentialStatus side and never
// decrypts.
type Credential struct {
	ID        uuid.UUID
	Host      string
	Token     string
	Validated bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store implements the credentials aggregate: token-only, keyed by unique
// host (docs/persistence-spec.md "credentials"). UpsertToken and GetByHost
// are the only methods that touch plaintext, via the AES-GCM encryptor
// (loam-54o.6) injected at construction; GetStatus, ListStatuses, and
// SetValidated never decrypt.
type Store struct {
	q      querier
	enc    encryptor
	logger *slog.Logger
}

// New builds a Store backed by pool and enc, typically internal/crypto's
// *Encryptor built from internal/config's Config.EncryptionKey. Callers
// must have already run migrations.Migrate against pool's DSN.
func New(pool *pgxpool.Pool, enc encryptor, logger *slog.Logger) *Store {
	return newStore(gen.New(pool), enc, logger)
}

// NewInTx builds a Store bound to tx, an already-open transaction the
// caller owns and will commit or roll back itself -- matching this repo's
// sibling stores (e.g. reviewstore.NewRoundStoreInTx). Store never calls
// tx.Begin/Commit/Rollback itself, so there is no nested-transaction path
// to guard against here.
func NewInTx(tx pgx.Tx, enc encryptor, logger *slog.Logger) *Store {
	return newStore(gen.New(tx), enc, logger)
}

// newStore is New's and NewInTx's unexported core, taking the querier seam
// directly so unit tests can supply a moq mock instead of a live pool or
// transaction.
func newStore(q querier, enc encryptor, logger *slog.Logger) *Store {
	return &Store{q: q, enc: enc, logger: logger}
}

// UpsertToken encrypts token via the injected AES-GCM encryptor and
// upserts it for host: an insert on first use, an in-place replace (never
// a second row) on every later call for the same host --
// credentials_host_key (UNIQUE(host)) is the constraint this relies on.
// The plaintext token never reaches the query layer or the database --
// only Encrypt's output does.
func (s *Store) UpsertToken(ctx context.Context, host, token string) (CredentialStatus, error) {
	ciphertext, err := s.enc.Encrypt([]byte(token))
	if err != nil {
		return CredentialStatus{}, fmt.Errorf("encrypting token for host %s: %w", host, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return CredentialStatus{}, fmt.Errorf("generating credential id for host %s: %w", host, err)
	}
	row, err := s.q.UpsertCredentialToken(ctx, gen.UpsertCredentialTokenParams{
		ID:              pgUUID(id),
		Host:            host,
		TokenCiphertext: ciphertext,
	})
	if err != nil {
		return CredentialStatus{}, fmt.Errorf("upserting token for host %s: %w", host, err)
	}
	s.logger.InfoContext(ctx, "upserted credential token", "host", host)
	return CredentialStatus{Host: row.Host, HasToken: row.TokenCiphertext != nil, Validated: row.Validated}, nil
}

// GetByHost returns host's credential with Token already decrypted via the
// injected AES-GCM encryptor, for callers that need the plaintext: git
// credential injection or a forge REST call (docs/sync-spec.md "Upstream
// Transport"). It returns ErrNotFound if host has no credentials row, and
// ErrNoToken if the row exists but token_ciphertext is null.
func (s *Store) GetByHost(ctx context.Context, host string) (Credential, error) {
	row, err := s.q.GetCredentialByHost(ctx, host)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Credential{}, fmt.Errorf("getting credential for host %s: %w", host, ErrNotFound)
		}
		return Credential{}, fmt.Errorf("getting credential for host %s: %w", host, err)
	}
	if row.TokenCiphertext == nil {
		return Credential{}, fmt.Errorf("getting credential for host %s: %w", host, ErrNoToken)
	}
	plaintext, err := s.enc.Decrypt(row.TokenCiphertext)
	if err != nil {
		return Credential{}, fmt.Errorf("decrypting token for host %s: %w", host, err)
	}
	return credentialFromRow(row, string(plaintext)), nil
}

// GetStatus returns host's presence and validation state without ever
// decrypting token_ciphertext -- backs
// CredentialService.GetCredentialStatus (docs/web-spec.md).
func (s *Store) GetStatus(ctx context.Context, host string) (CredentialStatus, error) {
	row, err := s.q.GetCredentialStatus(ctx, host)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CredentialStatus{}, fmt.Errorf("getting credential status for host %s: %w", host, ErrNotFound)
		}
		return CredentialStatus{}, fmt.Errorf("getting credential status for host %s: %w", host, err)
	}
	return CredentialStatus{Host: row.Host, HasToken: row.HasToken, Validated: row.Validated}, nil
}

// ListStatuses returns every host's presence and validation state, ordered
// by host, without ever decrypting token_ciphertext -- backs
// CredentialService.ListCredentials (docs/web-spec.md).
func (s *Store) ListStatuses(ctx context.Context) ([]CredentialStatus, error) {
	rows, err := s.q.ListCredentialStatuses(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing credential statuses: %w", err)
	}
	statuses := make([]CredentialStatus, 0, len(rows))
	for _, row := range rows {
		statuses = append(statuses, CredentialStatus{Host: row.Host, HasToken: row.HasToken, Validated: row.Validated})
	}
	return statuses, nil
}

// SetValidated updates host's validated flag, set by the provider's
// ValidateToken result (docs/web-spec.md "CredentialService":
// "SetUpstreamToken ... the server validates the REST side immediately").
// It never touches token_ciphertext.
func (s *Store) SetValidated(ctx context.Context, host string, validated bool) (CredentialStatus, error) {
	row, err := s.q.SetCredentialValidated(ctx, gen.SetCredentialValidatedParams{Host: host, Validated: validated})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CredentialStatus{}, fmt.Errorf("setting validated for host %s: %w", host, ErrNotFound)
		}
		return CredentialStatus{}, fmt.Errorf("setting validated for host %s: %w", host, err)
	}
	s.logger.InfoContext(ctx, "set credential validated", "host", host, "validated", validated)
	return CredentialStatus{Host: row.Host, HasToken: row.HasToken, Validated: row.Validated}, nil
}

// credentialFromRow converts a generated Credential row plus its
// already-decrypted plaintext into the package's own Credential type,
// keeping pgtype/gen details out of this package's public surface.
func credentialFromRow(row gen.Credential, token string) Credential {
	return Credential{
		ID:        uuidFromPg(row.ID),
		Host:      row.Host,
		Token:     token,
		Validated: row.Validated,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
}

// pgUUID adapts a uuid.UUID to the pgtype.UUID sqlc's generated params
// expect.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// uuidFromPg adapts a pgtype.UUID scanned off a generated row back to a
// uuid.UUID. credentials.id is NOT NULL (docs/persistence-spec.md
// "credentials"), so a row that scanned successfully always carries
// Valid: true here.
func uuidFromPg(id pgtype.UUID) uuid.UUID {
	return id.Bytes
}
