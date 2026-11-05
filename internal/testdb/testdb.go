// Package testdb is the integration-test database harness (plan 16.2, M1).
//
// Per test process it ensures the cluster roles (idempotent, using the role
// passwords from the environment), builds one template database migrated as
// studio_migrator, and clones a fresh database from it for each test. Clones
// and the template are dropped afterwards. Every application-level connection
// a test makes uses a real role login, never the superuser.
//
// Required environment (set by `make test-integration`):
//
//	STUDIO_TEST_DATABASE_URL       superuser URL (setup and teardown only)
//	STUDIO_DB_MIGRATOR_PASSWORD    login-role passwords, also used by
//	STUDIO_DB_APP_PASSWORD         `studioctl db bootstrap`
//	STUDIO_DB_WORKER_PASSWORD
//	STUDIO_DB_BACKUP_PASSWORD
//	STUDIO_DB_READONLY_PASSWORD
//
// Missing variables fail the test; they never cause a silent skip.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/bootstrap"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db/migrate"
)

// Admin is the pseudo-role for superuser URLs (setup, teardown and raw-SQL
// constraint checks that must not be blocked by privileges).
const Admin = "admin"

const setupTimeout = 2 * time.Minute

type harness struct {
	adminURL  *url.URL
	passwords bootstrap.Passwords
	template  string
}

var (
	setupOnce sync.Once
	setupErr  error
	h         harness
	cloneMu   sync.Mutex
)

// DB is one isolated test database.
type DB struct {
	Name string
}

// Main wraps testing.M: it runs the tests and then drops the template. Use it
// from TestMain in every package that uses this harness.
func Main(m *testing.M) int {
	code := m.Run()
	if h.template != "" {
		ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
		defer cancel()
		if err := dropDatabase(ctx, h.template); err != nil {
			fmt.Fprintf(os.Stderr, "testdb: drop template %s: %v\n", h.template, err)
		}
	}
	return code
}

func env() (*url.URL, bootstrap.Passwords, error) {
	raw := os.Getenv("STUDIO_TEST_DATABASE_URL")
	if raw == "" {
		return nil, bootstrap.Passwords{}, errors.New("STUDIO_TEST_DATABASE_URL is not set; run `make up` then `make test-integration`")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, bootstrap.Passwords{}, errors.New("STUDIO_TEST_DATABASE_URL is not a valid URL")
	}
	pw := bootstrap.Passwords{
		Migrator: os.Getenv("STUDIO_DB_MIGRATOR_PASSWORD"),
		App:      os.Getenv("STUDIO_DB_APP_PASSWORD"),
		Worker:   os.Getenv("STUDIO_DB_WORKER_PASSWORD"),
		Backup:   os.Getenv("STUDIO_DB_BACKUP_PASSWORD"),
		ReadOnly: os.Getenv("STUDIO_DB_READONLY_PASSWORD"),
	}
	if pw.ReadOnly == "" {
		return nil, bootstrap.Passwords{}, errors.New("STUDIO_DB_READONLY_PASSWORD is not set (tests log in as every role)")
	}
	if err := pw.Validate(); err != nil {
		return nil, bootstrap.Passwords{}, fmt.Errorf("STUDIO_DB_*_PASSWORD: %w", err)
	}
	return u, pw, nil
}

func setup() error {
	u, pw, err := env()
	if err != nil {
		return err
	}
	h.adminURL, h.passwords = u, pw

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, h.urlFor(Admin, "postgres"))
	if err != nil {
		return fmt.Errorf("connect as superuser: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if err := bootstrap.EnsureRoles(ctx, conn, pw); err != nil {
		return fmt.Errorf("ensure roles: %w", err)
	}

	tpl := uniqueName("studio_tpl")
	if err := bootstrap.EnsureDatabase(ctx, conn, tpl, ""); err != nil {
		return fmt.Errorf("create template: %w", err)
	}
	h.template = tpl
	if _, err := migrate.Up(ctx, h.urlFor(dbroles.Migrator, tpl)); err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}
	return nil
}

func (hh harness) urlFor(role, database string) string {
	u := *hh.adminURL
	u.Path = "/" + database
	switch role {
	case Admin:
	case dbroles.Migrator:
		u.User = url.UserPassword(role, hh.passwords.Migrator)
	case dbroles.App:
		u.User = url.UserPassword(role, hh.passwords.App)
	case dbroles.Worker:
		u.User = url.UserPassword(role, hh.passwords.Worker)
	case dbroles.Backup:
		u.User = url.UserPassword(role, hh.passwords.Backup)
	case dbroles.ReadOnly:
		u.User = url.UserPassword(role, hh.passwords.ReadOnly)
	default:
		panic("testdb: unknown role " + role)
	}
	return u.String()
}

func uniqueName(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s_%d_%s", prefix, os.Getpid(), hex.EncodeToString(b))
}

func ready(t testing.TB) {
	t.Helper()
	setupOnce.Do(func() { setupErr = setup() })
	if setupErr != nil {
		t.Fatalf("testdb: %v", setupErr)
	}
}

// New returns a fresh, fully migrated database cloned from the template.
func New(t testing.TB) *DB {
	t.Helper()
	ready(t)
	name := uniqueName("studio_t")
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	cloneMu.Lock()
	err := withAdmin(ctx, func(conn *pgx.Conn) error {
		return bootstrap.EnsureDatabase(ctx, conn, name, h.template)
	})
	cloneMu.Unlock()
	if err != nil {
		t.Fatalf("testdb: clone: %v", err)
	}
	t.Cleanup(func() { drop(t, name) })
	return &DB{Name: name}
}

// NewEmpty returns a freshly bootstrapped database with no migrations
// applied, for migrate-from-empty tests.
func NewEmpty(t testing.TB) *DB {
	t.Helper()
	ready(t)
	name := uniqueName("studio_e")
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	if err := withAdmin(ctx, func(conn *pgx.Conn) error {
		return bootstrap.EnsureDatabase(ctx, conn, name, "")
	}); err != nil {
		t.Fatalf("testdb: create empty: %v", err)
	}
	t.Cleanup(func() { drop(t, name) })
	return &DB{Name: name}
}

// URL returns a connection URL for this database as role (a dbroles login
// role, or Admin for the superuser).
func (d *DB) URL(role string) string { return h.urlFor(role, d.Name) }

// Connect opens a connection as role and closes it when the test ends.
func (d *DB) Connect(t testing.TB, role string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, d.URL(role))
	if err != nil {
		t.Fatalf("testdb: connect as %s: %v", role, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// Passwords returns the role passwords the harness bootstrapped with.
func Passwords(t testing.TB) bootstrap.Passwords {
	t.Helper()
	ready(t)
	return h.passwords
}

// AdminMaintenanceURL is the superuser URL to the maintenance database.
func AdminMaintenanceURL(t testing.TB) string {
	t.Helper()
	ready(t)
	return h.urlFor(Admin, "postgres")
}

func withAdmin(ctx context.Context, fn func(*pgx.Conn) error) error {
	conn, err := pgx.Connect(ctx, h.urlFor(Admin, "postgres"))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	return fn(conn)
}

func drop(t testing.TB, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	if err := dropDatabase(ctx, name); err != nil {
		t.Errorf("testdb: drop %s: %v", name, err)
	}
}

func dropDatabase(ctx context.Context, name string) error {
	return withAdmin(ctx, func(conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		return err
	})
}
