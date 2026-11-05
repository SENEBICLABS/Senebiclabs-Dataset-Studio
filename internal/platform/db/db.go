// Package db owns the PostgreSQL connection pool.
//
// M0 scope: open a pool and expose a readiness check. Roles, migrations and
// transaction helpers arrive in M1.
package db

import (
	"context"
	"errors"
	"fmt"

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
