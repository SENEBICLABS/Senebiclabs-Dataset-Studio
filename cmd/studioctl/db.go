package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/bootstrap"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/config"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db/migrate"
)

// Environment variables read by the database subcommands. They are separate
// from the STUDIO_* settings the services read, so no service process ever
// receives superuser or migrator credentials.
//
//nolint:gosec // G101: these constants are environment-variable NAMES, not credentials; values are read at runtime and never logged.
const (
	envBootstrapURL      = "STUDIO_BOOTSTRAP_DATABASE_URL" // superuser, maintenance database
	envDBName            = "STUDIO_DB_NAME"
	envMigratorURL       = "STUDIO_MIGRATOR_DATABASE_URL"
	envMigratorPassword  = "STUDIO_DB_MIGRATOR_PASSWORD"
	envAppPassword       = "STUDIO_DB_APP_PASSWORD"
	envWorkerPassword    = "STUDIO_DB_WORKER_PASSWORD"
	envBackupPassword    = "STUDIO_DB_BACKUP_PASSWORD"
	envReadOnlyPassword  = "STUDIO_DB_READONLY_PASSWORD"
	defaultDatabaseName  = "studio"
	databaseCommandLimit = 5 * time.Minute
)

func get(lookup config.Lookup, key string) string {
	v, _ := lookup(key)
	return strings.TrimSpace(v)
}

func requireURL(lookup config.Lookup, key string) (string, error) {
	raw := get(lookup, key)
	if raw == "" {
		return "", fmt.Errorf("%s is not set", key)
	}
	if err := config.ValidateDatabaseURL(raw); err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	return raw, nil
}

// runDBBootstrap creates the roles and the application database (plan 6.1).
func runDBBootstrap(ctx context.Context, lookup config.Lookup, stdout, stderr io.Writer) int {
	adminURL, err := requireURL(lookup, envBootstrapURL)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	name := get(lookup, envDBName)
	if name == "" {
		name = defaultDatabaseName
	}
	pw := bootstrap.Passwords{
		Migrator: get(lookup, envMigratorPassword),
		App:      get(lookup, envAppPassword),
		Worker:   get(lookup, envWorkerPassword),
		Backup:   get(lookup, envBackupPassword),
		ReadOnly: get(lookup, envReadOnlyPassword),
	}
	if err := pw.Validate(); err != nil {
		_, _ = fmt.Fprintf(stderr, "role passwords invalid (set %s, %s, %s, %s):\n%v\n",
			envMigratorPassword, envAppPassword, envWorkerPassword, envBackupPassword, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(ctx, databaseCommandLimit)
	defer cancel()
	conn, err := connect(ctx, adminURL)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "connect %s: %v\n", config.RedactURL(adminURL), err)
		return 1
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	var isSuper bool
	if err := conn.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&isSuper); err != nil || !isSuper {
		_, _ = fmt.Fprintf(stderr, "%s must connect as a superuser\n", envBootstrapURL)
		return 1
	}
	if err := bootstrap.EnsureRoles(ctx, conn, pw); err != nil {
		_, _ = fmt.Fprintf(stderr, "roles: %v\n", err)
		return 1
	}
	if err := bootstrap.EnsureDatabase(ctx, conn, name, ""); err != nil {
		_, _ = fmt.Fprintf(stderr, "database %q: %v\n", name, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "bootstrap complete: roles ensured, database %q owned by studio_owner\n", name)
	return 0
}

// runMigrate applies migrations and grants, or reports status.
func runMigrate(ctx context.Context, args []string, lookup config.Lookup, stdout, stderr io.Writer) int {
	action := "up"
	if len(args) > 0 {
		action = args[0]
	}
	if len(args) > 1 || (action != "up" && action != "status") {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	migratorURL, err := requireURL(lookup, envMigratorURL)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, databaseCommandLimit)
	defer cancel()

	if action == "status" {
		current, latest, err := migrate.Status(ctx, migratorURL)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "status: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "current version %d, latest %d\n", current, latest)
		return 0
	}
	res, err := migrate.Up(ctx, migratorURL)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "migrate: %v\n", err)
		return 1
	}
	if len(res.Applied) == 0 {
		_, _ = fmt.Fprintf(stdout, "up to date at version %d; grants re-applied\n", res.Version)
	} else {
		_, _ = fmt.Fprintf(stdout, "applied %v; now at version %d; grants applied\n", res.Applied, res.Version)
	}
	return 0
}

func connect(ctx context.Context, raw string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		return nil, errors.New("invalid connection string")
	}
	cfg.RuntimeParams["application_name"] = "studioctl"
	// pgconn connection errors name host, user and database, never the
	// password.
	return pgx.ConnectConfig(ctx, cfg)
}
