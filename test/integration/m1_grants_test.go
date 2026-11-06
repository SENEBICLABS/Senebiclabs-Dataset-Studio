//go:build integration

package integration

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/bootstrap"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/testdb"
)

func collect(t *testing.T, conn *pgx.Conn, sql string, args ...any) []string {
	t.Helper()
	rows, err := conn.Query(ctx(t), sql, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	slices.Sort(out)
	return out
}

func diff(t *testing.T, what string, got, want []string) {
	t.Helper()
	var missing, extra []string
	for _, w := range want {
		if !slices.Contains(got, w) {
			missing = append(missing, w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			extra = append(extra, g)
		}
	}
	if len(missing)+len(extra) > 0 {
		t.Errorf("%s drift:\n  missing: %v\n  unexpected: %v", what, missing, extra)
	}
}

// managedAndPublic is the SQL list of grantees whose privileges must match
// the map exactly. PUBLIC appears as grantee oid 0.
const grantees = `(SELECT oid FROM pg_roles WHERE rolname = ANY($1)) UNION ALL SELECT 0`

// tablePrivilegeKeys reads table- and column-level privileges held by the
// managed roles and PUBLIC on studio objects, as dbroles keys.
func tablePrivilegeKeys(t *testing.T, admin *pgx.Conn) []string {
	t.Helper()
	return collect(t, admin, `
		SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END
		       || '|' || c.relname || '|' || a.privilege_type || '|'
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'studio'
		CROSS JOIN LATERAL aclexplode(c.relacl) a
		WHERE a.grantee IN (`+grantees+`)
		UNION ALL
		SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END
		       || '|' || c.relname || '|' || a.privilege_type || '|' || att.attname
		FROM pg_attribute att
		JOIN pg_class c ON c.oid = att.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'studio'
		CROSS JOIN LATERAL aclexplode(att.attacl) a
		WHERE att.attnum > 0 AND NOT att.attisdropped AND a.grantee IN (`+grantees+`)`, dbroles.ManagedRoles)
}

// assertNoGrantDrift is G3: the live catalog equals the grants-as-code map.
func assertNoGrantDrift(t *testing.T, db *testdb.DB) {
	t.Helper()
	admin := db.Connect(t, testdb.Admin)
	managed := dbroles.ManagedRoles

	// pg_class holds tables and sequences, so sequence grants (M6) are
	// compared in the same set.
	wantRel := append(dbroles.ExpectedKeys(dbroles.Grants), dbroles.ExpectedObjectKeys(dbroles.ObjectGrants, dbroles.Sequence)...)
	slices.Sort(wantRel)
	diff(t, "table, column and sequence privileges", tablePrivilegeKeys(t, admin), wantRel)

	gotSchema := collect(t, admin, `
		SELECT n.nspname || '|' || CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END
		       || '|' || a.privilege_type
		FROM pg_namespace n
		CROSS JOIN LATERAL aclexplode(coalesce(n.nspacl, acldefault('n', n.nspowner))) a
		WHERE n.nspname IN ('studio', 'public') AND a.grantee IN (`+grantees+`)`, managed)
	var wantSchema []string
	for _, r := range dbroles.SchemaUsage {
		wantSchema = append(wantSchema, "studio|"+r+"|USAGE")
	}
	slices.Sort(wantSchema)
	diff(t, "schema privileges", gotSchema, wantSchema)

	gotFunc := collect(t, admin, `
		SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END
		       || '|' || p.proname || '|' || a.privilege_type || '|'
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace AND n.nspname = 'studio'
		CROSS JOIN LATERAL aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) a
		WHERE NOT EXISTS (SELECT 1 FROM pg_depend d
		                  WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
		  AND a.grantee IN (`+grantees+`)`, managed)
	diff(t, "function EXECUTE", gotFunc, dbroles.ExpectedObjectKeys(dbroles.ObjectGrants, dbroles.Function))

	// Enum and domain types (M6): PUBLIC's default USAGE is revoked.
	gotType := collect(t, admin, `
		SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END
		       || '|' || ty.typname || '|' || a.privilege_type || '|'
		FROM pg_type ty JOIN pg_namespace n ON n.oid = ty.typnamespace AND n.nspname = 'studio'
		CROSS JOIN LATERAL aclexplode(coalesce(ty.typacl, acldefault('T', ty.typowner))) a
		WHERE ty.typtype IN ('e', 'd')
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d
		                  WHERE d.classid = 'pg_type'::regclass AND d.objid = ty.oid AND d.deptype = 'e')
		  AND a.grantee IN (`+grantees+`)`, managed)
	diff(t, "type USAGE", gotType, dbroles.ExpectedObjectKeys(dbroles.ObjectGrants, dbroles.Type))

	gotDB := collect(t, admin, `
		SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END || '|' || a.privilege_type
		FROM pg_database d
		CROSS JOIN LATERAL aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) a
		WHERE d.datname = current_database() AND a.grantee IN (`+grantees+`)`, managed)
	var wantDB []string
	for _, r := range dbroles.LoginRoles {
		wantDB = append(wantDB, r+"|CONNECT")
	}
	slices.Sort(wantDB)
	diff(t, "database privileges", gotDB, wantDB)

	var dbOwner string
	if err := admin.QueryRow(ctx(t), `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = current_database()`).
		Scan(&dbOwner); err != nil || dbOwner != dbroles.Owner {
		t.Errorf("database owner = %q (%v), want %s", dbOwner, err, dbroles.Owner)
	}

	allRoles := append([]string{dbroles.Owner}, managed...)
	gotMembers := collect(t, admin, `
		SELECT pg_get_userbyid(m.member) || '>' || pg_get_userbyid(m.roleid)
		       || '|admin=' || m.admin_option || '|inherit=' || m.inherit_option || '|set=' || m.set_option
		FROM pg_auth_members m
		WHERE m.member IN (SELECT oid FROM pg_roles WHERE rolname = ANY($1))
		   OR m.roleid IN (SELECT oid FROM pg_roles WHERE rolname = ANY($1))`, allRoles)
	diff(t, "role memberships", gotMembers, []string{
		"studio_backup>pg_read_all_data|admin=false|inherit=true|set=true",
		"studio_migrator>studio_owner|admin=false|inherit=false|set=true",
	})

	gotAttrs := collect(t, admin, `
		SELECT rolname || '|login=' || rolcanlogin || '|super=' || rolsuper || '|createdb=' || rolcreatedb
		       || '|createrole=' || rolcreaterole || '|replication=' || rolreplication || '|bypassrls=' || rolbypassrls
		FROM pg_roles WHERE rolname = ANY($1)`, allRoles)
	var wantAttrs []string
	for _, r := range allRoles {
		login := r != dbroles.Owner
		wantAttrs = append(wantAttrs, r+"|login="+map[bool]string{true: "true", false: "false"}[login]+
			"|super=false|createdb=false|createrole=false|replication=false|bypassrls=false")
	}
	slices.Sort(wantAttrs)
	diff(t, "role attributes", gotAttrs, wantAttrs)

	gotSettings := collect(t, admin, `
		SELECT pg_get_userbyid(s.setrole) || '|' || cfg
		FROM pg_db_role_setting s CROSS JOIN LATERAL unnest(s.setconfig) cfg
		WHERE s.setdatabase = 0 AND s.setrole IN (SELECT oid FROM pg_roles WHERE rolname = ANY($1))`, allRoles)
	wantSettings := []string{
		"studio_app|idle_in_transaction_session_timeout=30s", "studio_app|search_path=studio", "studio_app|statement_timeout=15s",
		"studio_backup|idle_in_transaction_session_timeout=0", "studio_backup|search_path=studio", "studio_backup|statement_timeout=0",
		"studio_migrator|search_path=studio", "studio_owner|search_path=studio", "studio_readonly|search_path=studio",
		"studio_worker|idle_in_transaction_session_timeout=1min", "studio_worker|search_path=studio", "studio_worker|statement_timeout=5min",
	}
	slices.Sort(wantSettings)
	diff(t, "role settings", gotSettings, wantSettings)
}

// G3 on a cloned database: the template's grants survive cloning and the
// database-level access list is re-applied.
func TestG3GrantDrift(t *testing.T) {
	assertNoGrantDrift(t, testdb.New(t))
}

// Grants are idempotent: re-running bootstrap and migrate changes nothing,
// and a privilege added by hand is removed by the next migrate run.
func TestG3GrantsAreReassertedByMigrate(t *testing.T) {
	db := testdb.New(t)
	admin := db.Connect(t, testdb.Admin)
	if _, err := admin.Exec(ctx(t), `GRANT DELETE ON studio.principals TO studio_app`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx(t), `GRANT SELECT ON studio.users TO PUBLIC`); err != nil {
		t.Fatal(err)
	}
	// The detector sees both injected privileges before they are repaired.
	injected := tablePrivilegeKeys(t, admin)
	for _, k := range []string{"studio_app|principals|DELETE|", "PUBLIC|users|SELECT|"} {
		if !slices.Contains(injected, k) {
			t.Fatalf("drift detector missed injected privilege %s", k)
		}
	}

	maint, err := pgx.Connect(ctx(t), testdb.AdminMaintenanceURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = maint.Close(ctx(t)) }()
	if err := bootstrap.EnsureRoles(ctx(t), maint, testdb.Passwords(t)); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if err := bootstrap.EnsureDatabase(ctx(t), maint, db.Name, ""); err != nil {
		t.Fatalf("second EnsureDatabase: %v", err)
	}
	if _, err := migrateUp(t, db); err != nil {
		t.Fatal(err)
	}
	assertNoGrantDrift(t, db)
}

// G2: each role is refused what its specification forbids (plan 6.1, 6.3).
func TestG2RolesAreRefusedForbiddenOperations(t *testing.T) {
	db := testdb.New(t)
	forbidden := map[string][]string{
		dbroles.App: {
			`TRUNCATE studio.principals`,
			`DELETE FROM studio.principals`,
			`DELETE FROM studio.users`,
			`UPDATE studio.principals SET handle = 'x'`,
			`SELECT handle FROM studio.principals`,
			// M4: administration may change only display_name, global_role
			// and password_hash on users, and nothing on service accounts.
			`UPDATE studio.users SET email = 'x@example.test'`,
			`UPDATE studio.service_accounts SET global_role = 'platform_admin'`,
			`DELETE FROM studio.service_accounts`,
			`SELECT created_by FROM studio.users`,
			`SELECT * FROM studio.installation`,
			`SELECT * FROM studio.goose_db_version`,
			`CREATE TABLE studio.x (id int)`,
			`CREATE TABLE public.x (id int)`,
			`CREATE TEMP TABLE x (id int)`,
			`ALTER TABLE studio.principals DISABLE TRIGGER ALL`,
			`DROP TABLE studio.principals`,
			`SET ROLE studio_owner`,
			`SELECT studio.principals_forbid_identity_update()`,
		},
		dbroles.Worker: {
			`SELECT * FROM studio.users`,
			`SELECT id FROM studio.principals`,
			`SELECT key FROM studio.idempotency_keys`,
			`SELECT response_body FROM studio.idempotency_keys`,
			`INSERT INTO studio.principals (kind, handle, created_by) VALUES ('user', 'w', '00000000-0000-7000-8000-000000000001')`,
			`UPDATE studio.idempotency_keys SET response_status = 200`,
			`TRUNCATE studio.idempotency_keys`,
			`CREATE TABLE studio.x (id int)`,
			`SET ROLE studio_owner`,
		},
		dbroles.Backup: {
			`INSERT INTO studio.principals (kind, handle, created_by) VALUES ('user', 'b', '00000000-0000-7000-8000-000000000001')`,
			`UPDATE studio.principals SET status = 'disabled'`,
			`DELETE FROM studio.idempotency_keys`,
			`TRUNCATE studio.installation`,
			`CREATE TABLE studio.x (id int)`,
			`SET ROLE studio_owner`,
		},
		dbroles.ReadOnly: {
			`SELECT password_hash FROM studio.users`,
			`SELECT * FROM studio.users`,
			`SELECT * FROM studio.idempotency_keys`,
			`INSERT INTO studio.principals (kind, handle, created_by) VALUES ('user', 'r', '00000000-0000-7000-8000-000000000001')`,
			`UPDATE studio.principals SET status = 'disabled'`,
			`CREATE TABLE studio.x (id int)`,
			`SET ROLE studio_owner`,
		},
		// Without an explicit SET ROLE the migrator holds nothing.
		dbroles.Migrator: {
			`SELECT id FROM studio.principals`,
			`INSERT INTO studio.principals (kind, handle, created_by) VALUES ('user', 'm', '00000000-0000-7000-8000-000000000001')`,
			`CREATE TABLE studio.x (id int)`,
			`CREATE TABLE public.x (id int)`,
		},
	}
	for role, stmts := range forbidden {
		conn := db.Connect(t, role)
		for _, stmt := range stmts {
			wantSQLState(t, exec(t, conn, stmt), insufficientPriv, "", role+": "+stmt)
		}
	}

	// The migrator can become the owner only explicitly.
	m := db.Connect(t, dbroles.Migrator)
	wantOK(t, exec(t, m, `SET LOCAL ROLE studio_owner; CREATE TABLE studio.x (id int)`), "migrator after SET ROLE")
}

// The grants are usable: each role can do what the matrix gives it.
func TestRolesCanUseTheirGrants(t *testing.T) {
	db := testdb.New(t)
	app := db.Connect(t, dbroles.App)
	var userID string
	if err := app.QueryRow(ctx(t), `
		INSERT INTO studio.principals (kind, handle, created_by)
		VALUES ('user', 'alice', '00000000-0000-7000-8000-000000000001') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("app insert principal: %v", err)
	}
	if _, err := app.Exec(ctx(t), `
		INSERT INTO studio.users (principal_id, email, display_name, password_hash, created_by)
		VALUES ($1, 'Alice@Example.test', 'Alice', 'hash-placeholder', '00000000-0000-7000-8000-000000000001')`, userID); err != nil {
		t.Fatalf("app insert user: %v", err)
	}
	// citext comparison works for the app (extension functions stay callable).
	var found string
	if err := app.QueryRow(ctx(t), `SELECT principal_id::text FROM studio.users WHERE email = 'alice@example.TEST'`).
		Scan(&found); err != nil || found != userID {
		t.Fatalf("app case-insensitive email lookup: %q %v", found, err)
	}
	if _, err := app.Exec(ctx(t), `UPDATE studio.users SET display_name = 'A.', password_hash = 'h2' WHERE principal_id = $1`, userID); err != nil {
		t.Errorf("app update user: %v", err)
	}
	if _, err := app.Exec(ctx(t), `UPDATE studio.principals SET status = 'disabled' WHERE id = $1`, userID); err != nil {
		t.Errorf("app update principal status: %v", err)
	}
	if _, err := app.Exec(ctx(t), `
		INSERT INTO studio.idempotency_keys (principal_id, key, request_sha256, response_status, response_body, expires_at, created_by)
		VALUES ($1, 'k1', sha256('req'::bytea), 201, '{}'::bytea, now() - interval '1 hour', $1)`, userID); err != nil {
		t.Fatalf("app insert idempotency key: %v", err)
	}

	worker := db.Connect(t, dbroles.Worker)
	tag, err := worker.Exec(ctx(t), `DELETE FROM studio.idempotency_keys WHERE expires_at < now()`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Errorf("worker idempotency sweep: %v rows=%d", err, tag.RowsAffected())
	}

	backup := db.Connect(t, dbroles.Backup)
	var hash string
	if err := backup.QueryRow(ctx(t), `SELECT password_hash FROM studio.users WHERE principal_id = $1`, userID).Scan(&hash); err != nil {
		t.Errorf("backup reads everything: %v", err)
	}

	ro := db.Connect(t, dbroles.ReadOnly)
	var n int
	if err := ro.QueryRow(ctx(t), `SELECT count(*) FROM studio.principals`).Scan(&n); err != nil || n != 4 {
		t.Errorf("readonly principals count = %d, %v", n, err)
	}
	if err := ro.QueryRow(ctx(t), `SELECT count(*) FROM studio.users WHERE email = 'ALICE@example.test'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("readonly users (non-credential columns) = %d, %v", n, err)
	}
}

// Per-role settings take effect at login (plan 6.1).
func TestRoleSessionSettings(t *testing.T) {
	db := testdb.New(t)
	want := map[string][3]string{
		dbroles.App:      {"15s", "30s", "studio"},
		dbroles.Worker:   {"5min", "1min", "studio"},
		dbroles.Backup:   {"0", "0", "studio"},
		dbroles.ReadOnly: {"0", "0", "studio"},
		dbroles.Migrator: {"0", "0", "studio"},
	}
	for role, w := range want {
		conn := db.Connect(t, role)
		var stmt, idle, path string
		if err := conn.QueryRow(ctx(t), `SELECT current_setting('statement_timeout'),
			current_setting('idle_in_transaction_session_timeout'), current_setting('search_path')`).
			Scan(&stmt, &idle, &path); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if [3]string{stmt, idle, path} != w {
			t.Errorf("%s: settings = %s/%s/%s, want %v", role, stmt, idle, path, w)
		}
		var user string
		if err := conn.QueryRow(ctx(t), `SELECT current_user`).Scan(&user); err != nil || !strings.EqualFold(user, role) {
			t.Errorf("logged in as %q, want %s", user, role)
		}
	}
}
