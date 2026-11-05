// Package bootstrap performs the superuser-only setup (plan 6.1): it creates
// the cluster roles and the application database. It creates no rows in any
// application table; schema objects and seed rows come from migrations.
//
// Every step is idempotent.
package bootstrap

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	bootstrapsql "github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/deploy/bootstrap"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
)

// rolesLockKey serialises concurrent bootstrap runs (for example parallel test
// packages) on the cluster-wide role catalog.
const rolesLockKey = 0x5354554449_4f52 // "STUDIO" "R"

// MinPasswordLength is the minimum accepted login-role password length.
const MinPasswordLength = 12

// Passwords holds the login-role passwords. ReadOnly may be empty, in which
// case the optional studio_readonly role has no password and cannot log in
// with one.
type Passwords struct {
	Migrator, App, Worker, Backup, ReadOnly string
}

func (p Passwords) byRole() map[string]string {
	return map[string]string{
		dbroles.Migrator: p.Migrator,
		dbroles.App:      p.App,
		dbroles.Worker:   p.Worker,
		dbroles.Backup:   p.Backup,
		dbroles.ReadOnly: p.ReadOnly,
	}
}

// Validate checks the passwords without revealing them in errors.
func (p Passwords) Validate() error {
	var errs []error
	for role, pw := range p.byRole() {
		if pw == "" && role == dbroles.ReadOnly {
			continue
		}
		if err := validatePassword(pw); err != nil {
			errs = append(errs, fmt.Errorf("password for %s: %w", role, err))
		}
	}
	return errors.Join(errs...)
}

// Passwords are restricted to printable ASCII so the client-side SCRAM
// verifier matches what PostgreSQL derives after SASLprep normalisation.
func validatePassword(pw string) error {
	if len(pw) < MinPasswordLength {
		return fmt.Errorf("must be at least %d characters", MinPasswordLength)
	}
	for i := 0; i < len(pw); i++ {
		if pw[i] < 0x21 || pw[i] > 0x7e {
			return errors.New("must be printable ASCII without spaces")
		}
	}
	return nil
}

// EnsureRoles creates or updates the six roles, their attributes, memberships
// and settings, and sets login passwords as SCRAM-SHA-256 verifiers so the
// plaintext never reaches the server. conn must be a superuser connection.
func EnsureRoles(ctx context.Context, conn *pgx.Conn, pw Passwords) error {
	if err := pw.Validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(rolesLockKey)); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		if _, err := tx.Exec(ctx, bootstrapsql.RolesSQL); err != nil {
			return fmt.Errorf("roles: %w", err)
		}
		for _, role := range dbroles.LoginRoles {
			password := pw.byRole()[role]
			var stmt string
			if password == "" {
				stmt = fmt.Sprintf("ALTER ROLE %s PASSWORD NULL", pgx.Identifier{role}.Sanitize())
			} else {
				verifier, err := scramVerifier(password)
				if err != nil {
					return err
				}
				// The verifier is not secret-equivalent to the password, but it
				// is still never logged. %L quoting happens server-side.
				if err := tx.QueryRow(ctx, "SELECT format('ALTER ROLE %I PASSWORD %L', $1::text, $2::text)",
					role, verifier).Scan(&stmt); err != nil {
					return fmt.Errorf("format password statement for %s: %w", role, err)
				}
			}
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("set password for %s: %w", role, err)
			}
		}
		return nil
	})
}

// EnsureDatabase creates database name owned by studio_owner (optionally from
// template), or transfers ownership if it exists, and sets its access list:
// nothing for PUBLIC, CONNECT for the login roles. conn must be a superuser
// connection to a different (maintenance) database.
func EnsureDatabase(ctx context.Context, conn *pgx.Conn, name, template string) error {
	if name == "" {
		return errors.New("database name is required")
	}
	db := pgx.Identifier{name}.Sanitize()
	owner := pgx.Identifier{dbroles.Owner}.Sanitize()

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return fmt.Errorf("check database: %w", err)
	}
	if !exists {
		stmt := fmt.Sprintf("CREATE DATABASE %s OWNER %s", db, owner)
		if template != "" {
			stmt += " TEMPLATE " + pgx.Identifier{template}.Sanitize()
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("create database: %w", err)
		}
	} else if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", db, owner)); err != nil {
		return fmt.Errorf("set database owner: %w", err)
	}

	login := ""
	for i, r := range dbroles.LoginRoles {
		if i > 0 {
			login += ", "
		}
		login += pgx.Identifier{r}.Sanitize()
	}
	for _, stmt := range []string{
		fmt.Sprintf("REVOKE ALL ON DATABASE %s FROM PUBLIC", db),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", db, login),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("database access: %w", err)
		}
	}
	return nil
}

// scramIterations matches PostgreSQL's default scram_iterations.
const scramIterations = 4096

// scramVerifier returns a PostgreSQL SCRAM-SHA-256 verifier for password
// (RFC 5802 / RFC 7677), in the format PostgreSQL stores in pg_authid.
func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salt: %w", err)
	}
	return scramVerifierWithSalt(password, salt, scramIterations)
}

func scramVerifierWithSalt(password string, salt []byte, iterations int) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("derive key: %w", err)
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		iterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
