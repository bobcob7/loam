package db

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// NewPool builds a pgx connection pool from cfg and pings it, failing fast
// with a wrapped error if the DSN is malformed or the database is
// unreachable. Each pooled connection registers the pgvector types so
// `vector` columns can be scanned into pgvector-go's Vector type. The caller
// owns the returned pool and must Close it.
func NewPool(ctx context.Context, cfg Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("creating pool: %w", errMissingDatabaseURL)
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing database url: %w", err)
	}
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating connection pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	logger.InfoContext(ctx, "connected to database")
	return pool, nil
}
