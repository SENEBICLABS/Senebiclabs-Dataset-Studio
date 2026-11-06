// Package db owns the PostgreSQL connection pool.
//
// It opens the pool, exposes a readiness check and provides WithTx, the
// transaction helper every command and job uses (plan 4).
package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open parses url and creates a pool. pgxpool connects lazily, so Open does
// not fail when the server is down; readiness reports that instead.
func Open(ctx context.Context, url, applicationName string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx errors can include the connection string; do not wrap it.
		return nil, errors.New("parse database url: invalid connection string")
	}
	if applicationName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = applicationName
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

// PingChecker is a readiness check that round-trips to the server.
type PingChecker struct {
	Pool *pgxpool.Pool
}

// Name implements health.Checker.
func (PingChecker) Name() string { return "database" }

// Check implements health.Checker.
func (p PingChecker) Check(ctx context.Context) error {
	var one int
	if err := p.Pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	return nil
}

// Beginner is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// WithTx runs fn in one transaction and commits only if fn returns nil. Any
// error, including one returned by River inside fn, rolls the whole
// transaction back (plan 3.3 R4: River no longer uses savepoints, so a River
// error leaves the transaction unusable). A panic in fn also rolls back and
// is re-raised.
func WithTx(ctx context.Context, db Beginner, fn func(tx pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// Rollback after a successful Commit is a no-op. WithoutCancel lets the
	// rollback reach the server even when ctx has already expired.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
