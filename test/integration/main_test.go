//go:build integration

// Package integration holds database-level acceptance tests that span
// packages (plan 17). Each test gets its own cloned database from
// internal/testdb.
package integration

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db/migrate"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)
	return c
}

// exec runs one statement in its own transaction, rolled back afterwards so
// a rejected or accepted statement never affects the next assertion.
func exec(t *testing.T, conn *pgx.Conn, sql string, args ...any) error {
	t.Helper()
	tx, err := conn.Begin(ctx(t))
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(ctx(t), sql, args...)
	return err
}

// wantSQLState asserts err is a PostgreSQL error with the given SQLSTATE and,
// when constraint is non-empty, the given constraint name.
func wantSQLState(t *testing.T, err error, code, constraint, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("%s: expected SQLSTATE %s, got %v", what, code, err)
		return
	}
	if pgErr.Code != code || (constraint != "" && pgErr.ConstraintName != constraint) {
		t.Errorf("%s: got SQLSTATE %s constraint %q (%s), want %s %q",
			what, pgErr.Code, pgErr.ConstraintName, pgErr.Message, code, constraint)
	}
}

func wantOK(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: unexpected error: %v", what, err)
	}
}

// SQLSTATE codes used in assertions.
const (
	checkViolation      = "23514"
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	restrictViolation   = "23001"
	insufficientPriv    = "42501"
)

func migrateUp(t *testing.T, db *testdb.DB) (migrate.Result, error) {
	t.Helper()
	return migrate.Up(ctx(t), db.URL(dbroles.Migrator))
}
