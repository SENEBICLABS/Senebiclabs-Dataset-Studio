// Package migrate applies the embedded forward-only goose migrations and then
// the grants map (plan 6.1: grants are applied after every migration).
//
// It must be given a studio_migrator connection URL. Every connection switches
// to studio_owner with SET ROLE before doing anything, so all objects are
// owned by the non-login owner role.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/migrations"
)

// VersionTable is goose's bookkeeping table (exempt from created_by, plan 7.1).
const VersionTable = dbroles.Schema + ".goose_db_version"

// Result summarises a migration run.
type Result struct {
	Applied []int64 // versions applied by this run (empty when up to date)
	Version int64   // current version after the run
}

// Up applies pending migrations and re-applies grants.
func Up(ctx context.Context, migratorURL string) (Result, error) {
	db, err := open(migratorURL)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = db.Close() }()

	if err := prepareSchema(ctx, db); err != nil {
		return Result{}, err
	}
	p, err := provider(db)
	if err != nil {
		return Result{}, err
	}
	results, err := p.Up(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("apply migrations: %w", err)
	}
	if err := ApplyGrants(ctx, db); err != nil {
		return Result{}, err
	}
	version, err := p.GetDBVersion(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read version: %w", err)
	}
	res := Result{Version: version}
	for _, r := range results {
		res.Applied = append(res.Applied, r.Source.Version)
	}
	return res, nil
}

// Status reports the current and latest available versions.
func Status(ctx context.Context, migratorURL string) (current, latest int64, err error) {
	db, err := open(migratorURL)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = db.Close() }()
	p, err := provider(db)
	if err != nil {
		return 0, 0, err
	}
	sources := p.ListSources()
	if len(sources) > 0 {
		latest = sources[len(sources)-1].Version
	}
	exists, err := tableExists(ctx, db)
	if err != nil || !exists {
		return 0, latest, err
	}
	current, err = p.GetDBVersion(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("read version: %w", err)
	}
	return current, latest, nil
}

// LatestVersion is the highest embedded migration version, read from the
// NNNNN_name.sql file names.
func LatestVersion() (int64, error) {
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, n := range names {
		prefix, _, ok := strings.Cut(n, "_")
		if !ok {
			return 0, fmt.Errorf("migration %q has no version prefix", n)
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migration %q: invalid version", n)
		}
		latest = max(latest, v)
	}
	if latest == 0 {
		return 0, errors.New("no migrations embedded")
	}
	return latest, nil
}

// ApplyGrants resets and applies the privilege map in one transaction. db must
// be acting as studio_owner.
func ApplyGrants(ctx context.Context, db *sql.DB) error {
	if err := dbroles.Validate(dbroles.Grants); err != nil {
		return fmt.Errorf("grant map: %w", err)
	}
	if err := dbroles.ValidateObjects(dbroles.ObjectGrants); err != nil {
		return fmt.Errorf("object grant map: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("grants: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range dbroles.Statements(dbroles.Grants, dbroles.ObjectGrants) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("grants: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("grants: commit: %w", err)
	}
	return nil
}

func open(migratorURL string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(migratorURL)
	if err != nil {
		// pgx errors can echo the connection string; do not wrap.
		return nil, errors.New("migrator database url: invalid connection string")
	}
	cfg.RuntimeParams["application_name"] = "studioctl-migrate"
	db := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(func(ctx context.Context, c *pgx.Conn) error {
		if _, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{dbroles.Owner}.Sanitize()); err != nil {
			return fmt.Errorf("set role %s: %w", dbroles.Owner, err)
		}
		return nil
	}))
	// One connection keeps the whole run on a single owner session.
	db.SetMaxOpenConns(1)
	return db, nil
}

// prepareSchema creates the application schema before goose needs it for its
// version table, and removes PUBLIC's default rights on the public schema.
func prepareSchema(ctx context.Context, db *sql.DB) error {
	schema := pgx.Identifier{dbroles.Schema}.Sanitize()
	for _, stmt := range []string{
		"CREATE SCHEMA IF NOT EXISTS " + schema + " AUTHORIZATION " + pgx.Identifier{dbroles.Owner}.Sanitize(),
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("prepare schema: %w", err)
		}
	}
	return nil
}

func provider(db *sql.DB) (*goose.Provider, error) {
	store, err := database.NewStore(database.DialectPostgres, VersionTable)
	if err != nil {
		return nil, fmt.Errorf("goose store: %w", err)
	}
	p, err := goose.NewProvider("", db, migrations.FS,
		goose.WithStore(store),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return nil, fmt.Errorf("goose provider: %w", err)
	}
	return p, nil
}

func tableExists(ctx context.Context, db *sql.DB) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", VersionTable).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check version table: %w", err)
	}
	return exists, nil
}
