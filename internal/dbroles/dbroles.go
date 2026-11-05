// Package dbroles is the single source of truth for database privileges
// (plan section 6, "grants as code").
//
// Every privilege any role holds on an object in the studio schema is listed
// in Grants. Apply revokes everything first and then grants exactly this list,
// inside the caller's transaction, so it is idempotent and leaves no drift.
// A grant-drift test compares the live catalog with this list.
package dbroles

import (
	"fmt"
	"sort"
	"strings"
)

// Schema is the application schema.
const Schema = "studio"

// Role names (plan 6.1).
const (
	Owner    = "studio_owner"    // NOLOGIN; owns every object
	Migrator = "studio_migrator" // LOGIN; SET ROLE studio_owner, no inheritance
	App      = "studio_app"      // LOGIN; studio-api
	Worker   = "studio_worker"   // LOGIN; studio-worker
	Backup   = "studio_backup"   // LOGIN; member of pg_read_all_data, no writes
	ReadOnly = "studio_readonly" // LOGIN; optional local debugging
)

// LoginRoles are the roles that may connect to the database.
var LoginRoles = []string{Migrator, App, Worker, Backup, ReadOnly}

// ManagedRoles are the grantees whose privileges this package controls. The
// owner is excluded (it owns the objects); PUBLIC is included and must hold
// nothing.
var ManagedRoles = []string{Migrator, App, Worker, Backup, ReadOnly}

// Grant is one privilege on one table, optionally restricted to columns.
type Grant struct {
	Role      string
	Table     string   // unqualified, in Schema
	Privilege string   // SELECT, INSERT, UPDATE or DELETE
	Columns   []string // empty means table-level
}

// SchemaUsage lists the roles granted USAGE on Schema. studio_backup reads
// through pg_read_all_data and studio_migrator acts as the owner, so neither
// needs it.
var SchemaUsage = []string{App, Worker, ReadOnly}

// Grants is the complete privilege map for the tables that exist so far. Each
// milestone that adds tables extends it (plan 6.3). Rows cite the matrix row
// they implement.
var Grants = []Grant{
	// studio_app: "every authenticated request" reads principals(id, kind,
	// status); user and service-account administration inserts principals and
	// updates their status.
	{Role: App, Table: "principals", Privilege: "SELECT", Columns: []string{"id", "kind", "status"}},
	{Role: App, Table: "principals", Privilege: "INSERT"},
	{Role: App, Table: "principals", Privilege: "UPDATE", Columns: []string{"status"}},
	// studio_app: auth/login reads users(principal_id, email, password_hash);
	// the matrix's "status" is principals.status, granted above.
	{Role: App, Table: "users", Privilege: "SELECT", Columns: []string{"principal_id", "email", "password_hash"}},
	{Role: App, Table: "users", Privilege: "INSERT"},
	{Role: App, Table: "users", Privilege: "UPDATE", Columns: []string{"display_name", "global_role", "password_hash"}},
	{Role: App, Table: "service_accounts", Privilege: "INSERT"},
	// studio_app: S/I idempotency_keys on POST.
	{Role: App, Table: "idempotency_keys", Privilege: "SELECT"},
	{Role: App, Table: "idempotency_keys", Privilege: "INSERT"},

	// studio_worker: idempotency.sweep is S(expires_at), D.
	{Role: Worker, Table: "idempotency_keys", Privilege: "SELECT", Columns: []string{"expires_at"}},
	{Role: Worker, Table: "idempotency_keys", Privilege: "DELETE"},

	// studio_readonly: SELECT, excluding credentials. users.password_hash is
	// withheld, and idempotency_keys is withheld entirely because stored
	// responses can contain issued secrets (for example a new API key).
	{Role: ReadOnly, Table: "principals", Privilege: "SELECT"},
	{Role: ReadOnly, Table: "service_accounts", Privilege: "SELECT"},
	{Role: ReadOnly, Table: "installation", Privilege: "SELECT"},
	{Role: ReadOnly, Table: "goose_db_version", Privilege: "SELECT"},
	{Role: ReadOnly, Table: "users", Privilege: "SELECT", Columns: []string{
		"principal_id", "kind", "email", "display_name", "global_role", "created_at", "created_by",
	}},
}

// Validate reports structural problems in the map (unknown roles or
// privileges, duplicates). It is exercised by unit tests.
func Validate(grants []Grant) error {
	managed := map[string]bool{}
	for _, r := range ManagedRoles {
		managed[r] = true
	}
	seen := map[string]bool{}
	for i, g := range grants {
		if !managed[g.Role] {
			return fmt.Errorf("grant %d: role %q is not a managed role", i, g.Role)
		}
		switch g.Privilege {
		case "SELECT", "INSERT", "UPDATE", "DELETE":
		default:
			return fmt.Errorf("grant %d: privilege %q is not allowed", i, g.Privilege)
		}
		if g.Privilege == "DELETE" && len(g.Columns) > 0 {
			return fmt.Errorf("grant %d: DELETE cannot be column-level", i)
		}
		if g.Table == "" || strings.Contains(g.Table, ".") {
			return fmt.Errorf("grant %d: table must be an unqualified name", i)
		}
		for _, k := range Keys(g) {
			if seen[k] {
				return fmt.Errorf("grant %d: duplicate privilege %s", i, k)
			}
			seen[k] = true
		}
	}
	return nil
}

// Keys returns the normalised privilege keys a grant produces, in the form
// "role|table|PRIVILEGE|column" (column empty for table-level). The drift test
// compares these with keys read from the catalog.
func Keys(g Grant) []string {
	if len(g.Columns) == 0 {
		return []string{fmt.Sprintf("%s|%s|%s|", g.Role, g.Table, g.Privilege)}
	}
	out := make([]string, 0, len(g.Columns))
	for _, c := range g.Columns {
		out = append(out, fmt.Sprintf("%s|%s|%s|%s", g.Role, g.Table, g.Privilege, c))
	}
	return out
}

// ExpectedKeys returns the sorted privilege keys for grants.
func ExpectedKeys(grants []Grant) []string {
	var out []string
	for _, g := range grants {
		out = append(out, Keys(g)...)
	}
	sort.Strings(out)
	return out
}

func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Statements returns the SQL that resets and applies the privilege map. Run it
// in one transaction as the schema owner.
func Statements(grants []Grant) []string {
	revokeFrom := "PUBLIC"
	for _, r := range ManagedRoles {
		revokeFrom += ", " + ident(r)
	}
	stmts := []string{
		// Table-level REVOKE also revokes column-level privileges.
		fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA %s FROM %s", ident(Schema), revokeFrom),
		fmt.Sprintf("REVOKE ALL ON ALL SEQUENCES IN SCHEMA %s FROM %s", ident(Schema), revokeFrom),
		fmt.Sprintf("REVOKE ALL ON SCHEMA %s FROM %s", ident(Schema), revokeFrom),
		// Our own functions (not extension members) are never callable by
		// PUBLIC. Extension functions (citext) keep their defaults because
		// the citext operators depend on them.
		fmt.Sprintf(`DO $$
DECLARE f regprocedure;
BEGIN
    FOR f IN
        SELECT p.oid::regprocedure FROM pg_proc p
        WHERE p.pronamespace = %s::regnamespace
          AND NOT EXISTS (SELECT 1 FROM pg_depend d
                          WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
    LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %%s FROM PUBLIC', f);
    END LOOP;
END $$`, quoteLiteral(Schema)),
	}
	for _, r := range SchemaUsage {
		stmts = append(stmts, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", ident(Schema), ident(r)))
	}
	for _, g := range grants {
		cols := ""
		if len(g.Columns) > 0 {
			quoted := make([]string, len(g.Columns))
			for i, c := range g.Columns {
				quoted[i] = ident(c)
			}
			cols = " (" + strings.Join(quoted, ", ") + ")"
		}
		stmts = append(stmts, fmt.Sprintf("GRANT %s%s ON %s.%s TO %s",
			g.Privilege, cols, ident(Schema), ident(g.Table), ident(g.Role)))
	}
	return stmts
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
