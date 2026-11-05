//go:build integration

package integration

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db/migrate"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/testdb"
)

const (
	rootID   = principals.RootID
	otherID  = "0190a6e4-0000-7000-8000-00000000abcd"
	otherID2 = "0190a6e4-0000-7000-8000-00000000abce"
)

// P1: migrations run from an empty database, as studio_migrator, and a second
// run is a no-op that still leaves the grants exact.
func TestP1MigrateFromEmpty(t *testing.T) {
	db := testdb.NewEmpty(t)
	latest, err := migrate.LatestVersion()
	if err != nil {
		t.Fatal(err)
	}

	res, err := migrate.Up(ctx(t), db.URL(dbroles.Migrator))
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if res.Version != latest || int64(len(res.Applied)) != latest {
		t.Fatalf("first run: %+v, want all %d migrations applied", res, latest)
	}

	res, err = migrate.Up(ctx(t), db.URL(dbroles.Migrator))
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(res.Applied) != 0 || res.Version != latest {
		t.Fatalf("second run must be a no-op: %+v", res)
	}
	current, l, err := migrate.Status(ctx(t), db.URL(dbroles.Migrator))
	if err != nil || current != latest || l != latest {
		t.Fatalf("status = %d/%d, %v", current, l, err)
	}

	// Every object is owned by studio_owner, never by the migrator login.
	admin := db.Connect(t, testdb.Admin)
	var notOwned []string
	rows, err := admin.Query(ctx(t), `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'studio' AND pg_get_userbyid(c.relowner) <> 'studio_owner'
		UNION ALL
		SELECT p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'studio' AND pg_get_userbyid(p.proowner) NOT IN ('studio_owner', current_user)
		UNION ALL
		SELECT nspname FROM pg_namespace WHERE nspname = 'studio' AND pg_get_userbyid(nspowner) <> 'studio_owner'`)
	if err != nil {
		t.Fatal(err)
	}
	notOwned, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(notOwned) > 0 {
		t.Errorf("objects not owned by studio_owner: %v", notOwned)
	}
	assertNoGrantDrift(t, db)
}

// Only studio_migrator can run migrations: other logins cannot become the
// owner.
func TestMigrateRequiresMigratorRole(t *testing.T) {
	db := testdb.NewEmpty(t)
	for _, role := range []string{dbroles.App, dbroles.Worker, dbroles.Backup, dbroles.ReadOnly} {
		if _, err := migrate.Up(ctx(t), db.URL(role)); err == nil {
			t.Errorf("%s was able to run migrations", role)
		}
	}
}

// P2: every studio table carries created_by uuid NOT NULL referencing
// principals(id), except the plan's exempt list (7.1).
func TestP2CreatedByOnEveryTable(t *testing.T) {
	db := testdb.New(t)
	admin := db.Connect(t, testdb.Admin)
	exempt := map[string]bool{
		"audit_events": true, "sessions": true, "resolved_schemas": true, "goose_db_version": true,
	}
	rows, err := admin.Query(ctx(t), `
		SELECT c.relname,
		       EXISTS (SELECT 1 FROM pg_attribute a
		               WHERE a.attrelid = c.oid AND a.attname = 'created_by' AND a.attnotnull
		                 AND a.atttypid = 'uuid'::regtype AND NOT a.attisdropped),
		       EXISTS (SELECT 1 FROM pg_constraint k
		               JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = k.conkey[1]
		               WHERE k.conrelid = c.oid AND k.contype = 'f' AND cardinality(k.conkey) = 1
		                 AND a.attname = 'created_by' AND k.confrelid = 'studio.principals'::regclass)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'studio' AND c.relkind IN ('r', 'p')
		ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for rows.Next() {
		var table string
		var hasColumn, hasFK bool
		if err := rows.Scan(&table, &hasColumn, &hasFK); err != nil {
			t.Fatal(err)
		}
		if exempt[table] || len(table) >= 6 && table[:6] == "river_" {
			continue
		}
		checked++
		if !hasColumn || !hasFK {
			t.Errorf("%s: created_by uuid NOT NULL = %v, FK to principals = %v", table, hasColumn, hasFK)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if checked < 5 {
		t.Errorf("only %d tables checked; expected at least the 5 M1 tables", checked)
	}
}

// The migration seeds exactly the three system principals and the installation
// row (plan 7.1).
func TestSeededSystemPrincipalsAndInstallation(t *testing.T) {
	db := testdb.New(t)
	admin := db.Connect(t, testdb.Admin)

	type row struct{ ID, Kind, Handle, Status, CreatedBy string }
	rows, err := admin.Query(ctx(t), `
		SELECT id::text, kind::text, handle, status, created_by::text FROM studio.principals ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		t.Fatal(err)
	}
	want := []row{
		{principals.RootID, "system", principals.RootHandle, "active", principals.RootID},
		{principals.CLIID, "system", principals.CLIHandle, "active", principals.RootID},
		{principals.WorkerID, "system", principals.WorkerHandle, "active", principals.RootID},
	}
	if !slices.Equal(got, want) {
		t.Errorf("seeded principals:\n got %+v\nwant %+v", got, want)
	}

	var n int
	var installationID, createdBy string
	if err := admin.QueryRow(ctx(t), `
		SELECT count(*) OVER (), installation_id::text, created_by::text FROM studio.installation`).
		Scan(&n, &installationID, &createdBy); err != nil {
		t.Fatal(err)
	}
	if n != 1 || installationID == "" || createdBy != principals.RootID {
		t.Errorf("installation: count %d id %q created_by %q", n, installationID, createdBy)
	}
	wantSQLState(t, exec(t, admin, `INSERT INTO studio.installation (created_by) VALUES ($1)`, rootID),
		uniqueViolation, "installation_pkey", "second installation row")
	wantSQLState(t, exec(t, admin, `INSERT INTO studio.installation (singleton, created_by) VALUES (false, $1)`, rootID),
		checkViolation, "installation_singleton_check", "non-singleton installation row")
}

// P3a: the root principal must reference itself and be of kind system.
func TestP3aRootMustSelfReferenceAndBeSystem(t *testing.T) {
	db := testdb.New(t)
	admin := db.Connect(t, testdb.Admin)

	// CHECK constraints are evaluated before the primary-key/unique checks, so
	// these inserts are rejected by the root CHECKs even though the root id
	// is already taken.
	wantSQLState(t, exec(t, admin,
		`INSERT INTO studio.principals (id, kind, handle, created_by) VALUES ($1, 'system', 'system:migration', $2)`,
		rootID, principals.CLIID),
		checkViolation, "principals_root_self_reference", "root with created_by <> id")
	wantSQLState(t, exec(t, admin,
		`INSERT INTO studio.principals (id, kind, handle, created_by) VALUES ($1, 'user', 'system:migration', $1)`,
		rootID),
		checkViolation, "principals_root_is_system", "root of kind user")
	wantSQLState(t, exec(t, admin,
		`INSERT INTO studio.principals (id, kind, handle, created_by) VALUES ($1, 'service_account', 'system:migration', $1)`,
		rootID),
		checkViolation, "principals_root_is_system", "root of kind service_account")

	// The seeded root satisfies all of them.
	var selfRef bool
	var kind string
	if err := admin.QueryRow(ctx(t), `SELECT created_by = id, kind::text FROM studio.principals WHERE handle = 'system:migration'`).
		Scan(&selfRef, &kind); err != nil {
		t.Fatal(err)
	}
	if !selfRef || kind != "system" {
		t.Errorf("seeded root: self-reference %v kind %s", selfRef, kind)
	}
}

// P3 / P3b: no non-root principal may reference itself (extends the Rev 2.1
// P3 test with the Rev 2.2 equivalence direction).
func TestP3bNonRootCannotSelfReference(t *testing.T) {
	db := testdb.New(t)
	admin := db.Connect(t, testdb.Admin)
	for _, kind := range []string{"user", "service_account", "system"} {
		wantSQLState(t, exec(t, admin,
			`INSERT INTO studio.principals (id, kind, handle, created_by) VALUES ($1, $2::studio.actor_kind, 'self-made', $1)`,
			otherID, kind),
			checkViolation, "principals_root_self_reference", "non-root self-reference, kind "+kind)
	}
	// The same row created by another principal is accepted.
	wantOK(t, exec(t, admin,
		`INSERT INTO studio.principals (id, kind, handle, created_by) VALUES ($1, 'user', 'self-made', $2)`,
		otherID, rootID), "non-root created by root")
}

// P3c: exactly one root, at the fixed id.
func TestP3cExactlyOneRootAtFixedID(t *testing.T) {
	db := testdb.New(t)
	admin := db.Connect(t, testdb.Admin)

	wantSQLState(t, exec(t, admin,
		`INSERT INTO studio.principals (id, kind, handle, created_by) VALUES ($1, 'system', 'system:migration', $1)`,
		otherID),
		checkViolation, "principals_root_fixed_id", "root at another id")
	wantSQLState(t, exec(t, admin,
		`INSERT INTO studio.principals (id, kind, handle, created_by) VALUES ($1, 'system', 'not-root', $2)`,
		rootID, principals.CLIID),
		checkViolation, "principals_root_fixed_id", "non-root row at the root id")
	wantSQLState(t, exec(t, admin,
		`INSERT INTO studio.principals (id, kind, handle, created_by) VALUES ($1, 'system', 'system:migration', $1)`,
		rootID),
		uniqueViolation, "principals_pkey", "second system:migration row")

	var roots int
	if err := admin.QueryRow(ctx(t), `SELECT count(*) FROM studio.principals WHERE created_by = id`).Scan(&roots); err != nil {
		t.Fatal(err)
	}
	if roots != 1 {
		t.Errorf("%d self-referencing principals, want exactly 1", roots)
	}
}

// P3d: id, handle, kind and created_by are immutable on every principal.
func TestP3dIdentityColumnsAreImmutable(t *testing.T) {
	db := testdb.New(t)
	admin := db.Connect(t, testdb.Admin)
	if _, err := admin.Exec(ctx(t),
		`INSERT INTO studio.principals (id, kind, handle, created_by) VALUES ($1, 'user', 'p3d-user', $2)`,
		otherID, rootID); err != nil {
		t.Fatal(err)
	}
	updates := map[string]string{
		"id":         `UPDATE studio.principals SET id = $2 WHERE id = $1`,
		"handle":     `UPDATE studio.principals SET handle = 'renamed' || $2::text WHERE id = $1`,
		"kind":       `UPDATE studio.principals SET kind = 'service_account' WHERE id = $1 AND $2::text IS NOT NULL`,
		"created_by": `UPDATE studio.principals SET created_by = $2 WHERE id = $1`,
	}
	for _, target := range []struct{ name, id, other string }{
		{"root", rootID, principals.CLIID},
		{"non-root", otherID, principals.CLIID},
	} {
		for col, sql := range updates {
			arg := target.other
			if col == "id" {
				arg = otherID2
			}
			wantSQLState(t, exec(t, admin, sql, target.id, arg),
				restrictViolation, "", "update "+col+" on "+target.name)
		}
	}
	// Mutable columns remain updatable.
	wantOK(t, exec(t, admin, `UPDATE studio.principals SET status = 'disabled' WHERE id = $1`, otherID), "update status")
}

// P4: system principals can never become users or service accounts, and a
// credential row must match its principal's kind.
func TestP4SystemPrincipalsCannotAuthenticate(t *testing.T) {
	db := testdb.New(t)
	admin := db.Connect(t, testdb.Admin)
	if _, err := admin.Exec(ctx(t), `
		INSERT INTO studio.principals (id, kind, handle, created_by) VALUES
			($1, 'user', 'p4-user', $3), ($2, 'service_account', 'p4-svc', $3)`,
		otherID, otherID2, rootID); err != nil {
		t.Fatal(err)
	}

	for _, sys := range []string{principals.RootID, principals.CLIID, principals.WorkerID} {
		wantSQLState(t, exec(t, admin,
			`INSERT INTO studio.users (principal_id, email, display_name, created_by) VALUES ($1, 'sys@example.test', 'x', $2)`,
			sys, rootID),
			foreignKeyViolation, "users_principal_fkey", "user row for system principal "+sys)
		wantSQLState(t, exec(t, admin,
			`INSERT INTO studio.users (principal_id, kind, email, display_name, created_by) VALUES ($1, 'system', 'sys@example.test', 'x', $2)`,
			sys, rootID),
			checkViolation, "users_kind_check", "user row of kind system for "+sys)
		wantSQLState(t, exec(t, admin,
			`INSERT INTO studio.service_accounts (principal_id, created_by) VALUES ($1, $2)`, sys, rootID),
			foreignKeyViolation, "service_accounts_principal_fkey", "service account for system principal "+sys)
		wantSQLState(t, exec(t, admin,
			`INSERT INTO studio.service_accounts (principal_id, kind, created_by) VALUES ($1, 'system', $2)`, sys, rootID),
			checkViolation, "service_accounts_kind_check", "service account of kind system for "+sys)
	}

	// Kinds must match across tables.
	wantSQLState(t, exec(t, admin,
		`INSERT INTO studio.users (principal_id, email, display_name, created_by) VALUES ($1, 'svc@example.test', 'x', $2)`,
		otherID2, rootID),
		foreignKeyViolation, "users_principal_fkey", "user row for a service-account principal")
	wantSQLState(t, exec(t, admin,
		`INSERT INTO studio.service_accounts (principal_id, created_by) VALUES ($1, $2)`, otherID, rootID),
		foreignKeyViolation, "service_accounts_principal_fkey", "service account for a user principal")

	// And the matching rows are accepted.
	wantOK(t, exec(t, admin,
		`INSERT INTO studio.users (principal_id, email, display_name, created_by) VALUES ($1, 'u@example.test', 'U', $2)`,
		otherID, rootID), "user row for user principal")
	wantOK(t, exec(t, admin,
		`INSERT INTO studio.service_accounts (principal_id, created_by) VALUES ($1, $2)`, otherID2, rootID),
		"service account for service-account principal")

	// principals reject the anonymous kind outright.
	wantSQLState(t, exec(t, admin,
		`INSERT INTO studio.principals (id, kind, handle, created_by) VALUES ($1, 'anonymous', 'anon', $2)`,
		otherID2, rootID),
		checkViolation, "principals_kind_not_anonymous", "anonymous principal")
}
