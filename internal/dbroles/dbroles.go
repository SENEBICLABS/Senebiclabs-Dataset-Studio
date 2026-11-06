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
	Privilege string   // SELECT, INSERT, UPDATE, DELETE or MAINTAIN
	Columns   []string // empty means table-level
}

// ObjectKind is a non-table object kind that carries grants.
type ObjectKind string

// Object kinds (M6: River's sequences, function and enum type, plan 6.3 R5
// and R7).
const (
	Sequence ObjectKind = "SEQUENCE"
	Function ObjectKind = "FUNCTION"
	Type     ObjectKind = "TYPE"
)

// ObjectGrant is one privilege on a sequence, function or type in Schema.
type ObjectGrant struct {
	Role      string
	Kind      ObjectKind
	Name      string // unqualified
	Args      string // functions only: the argument types, schema-qualified where needed
	Privilege string // USAGE for sequences and types, EXECUTE for functions
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
	// updates their status. handle stays unreadable (G2 asserts it).
	{Role: App, Table: "principals", Privilege: "SELECT", Columns: []string{"id", "kind", "status"}},
	{Role: App, Table: "principals", Privilege: "INSERT"},
	{Role: App, Table: "principals", Privilege: "UPDATE", Columns: []string{"status"}},
	// studio_app: auth/login reads users(principal_id, email, password_hash);
	// the matrix's "status" is principals.status, granted above. From M4,
	// every authenticated request reads global_role (the authorization
	// input), and the user administration reads (GET /v1/users) read
	// display_name and created_at.
	{Role: App, Table: "users", Privilege: "SELECT", Columns: []string{
		"principal_id", "email", "password_hash", "global_role", "display_name", "created_at",
	}},
	{Role: App, Table: "users", Privilege: "INSERT"},
	{Role: App, Table: "users", Privilege: "UPDATE", Columns: []string{"display_name", "global_role", "password_hash"}},
	// service_accounts holds no credentials; its global_role is read on every
	// API-key request and the whole row by GET /v1/service-accounts (M4).
	{Role: App, Table: "service_accounts", Privilege: "SELECT"},
	{Role: App, Table: "service_accounts", Privilege: "INSERT"},
	// studio_app: S/I idempotency_keys on POST.
	{Role: App, Table: "idempotency_keys", Privilege: "SELECT"},
	{Role: App, Table: "idempotency_keys", Privilege: "INSERT"},

	// sessions and api_keys (plan 6.3): every authenticated request reads
	// them (S) and refreshes sessions.last_seen_at; login inserts sessions;
	// logout and key revocation set revoked_at; administration inserts API
	// keys. No other role can see them (credential tables).
	{Role: App, Table: "sessions", Privilege: "SELECT"},
	{Role: App, Table: "sessions", Privilege: "INSERT"},
	{Role: App, Table: "sessions", Privilege: "UPDATE", Columns: []string{"last_seen_at", "revoked_at"}},
	{Role: App, Table: "api_keys", Privilege: "SELECT"},
	{Role: App, Table: "api_keys", Privilege: "INSERT"},
	{Role: App, Table: "api_keys", Privilege: "UPDATE", Columns: []string{"revoked_at"}},

	// audit_events (plan 6.3): every mutating row is "I audit_events"; the
	// audit query is "S audit_events". Append-only: no role holds UPDATE or
	// DELETE, and a trigger rejects them for everyone.
	{Role: App, Table: "audit_events", Privilege: "SELECT"},
	{Role: App, Table: "audit_events", Privilege: "INSERT"},
	{Role: Worker, Table: "audit_events", Privilege: "INSERT"},
	{Role: ReadOnly, Table: "audit_events", Privilege: "SELECT"},

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

	// River (M6, plan 6.3). studio_app: "River insert (R5): S, I, U(kind)
	// river_job"; the S also serves "Audit query, job list: S river_job".
	// No DELETE and no UPDATE beyond kind ("explicitly withheld").
	{Role: App, Table: "river_job", Privilege: "SELECT"},
	{Role: App, Table: "river_job", Privilege: "INSERT"},
	{Role: App, Table: "river_job", Privilege: "UPDATE", Columns: []string{"kind"}},
	// studio_worker: "Every job (fence, R1-R3): S, U river_job" and "River
	// client internals (R7, R8): S/I/U/D river_job, river_queue,
	// river_leader, river_notification; S river_migration; MAINTAIN on
	// river_job" (the reindexer).
	{Role: Worker, Table: "river_job", Privilege: "SELECT"},
	{Role: Worker, Table: "river_job", Privilege: "INSERT"},
	{Role: Worker, Table: "river_job", Privilege: "UPDATE"},
	{Role: Worker, Table: "river_job", Privilege: "DELETE"},
	{Role: Worker, Table: "river_job", Privilege: "MAINTAIN"},
	{Role: Worker, Table: "river_queue", Privilege: "SELECT"},
	{Role: Worker, Table: "river_queue", Privilege: "INSERT"},
	{Role: Worker, Table: "river_queue", Privilege: "UPDATE"},
	{Role: Worker, Table: "river_queue", Privilege: "DELETE"},
	{Role: Worker, Table: "river_leader", Privilege: "SELECT"},
	{Role: Worker, Table: "river_leader", Privilege: "INSERT"},
	{Role: Worker, Table: "river_leader", Privilege: "UPDATE"},
	{Role: Worker, Table: "river_leader", Privilege: "DELETE"},
	{Role: Worker, Table: "river_notification", Privilege: "SELECT"},
	{Role: Worker, Table: "river_notification", Privilege: "INSERT"},
	{Role: Worker, Table: "river_notification", Privilege: "UPDATE"},
	{Role: Worker, Table: "river_notification", Privilege: "DELETE"},
	{Role: Worker, Table: "river_migration", Privilege: "SELECT"},
	// studio_readonly: SELECT on non-credential tables (plan 6.1). River's
	// tables hold job arguments and errors, never credentials.
	{Role: ReadOnly, Table: "river_job", Privilege: "SELECT"},
	{Role: ReadOnly, Table: "river_queue", Privilege: "SELECT"},
	{Role: ReadOnly, Table: "river_leader", Privilege: "SELECT"},
	{Role: ReadOnly, Table: "river_notification", Privilege: "SELECT"},
	{Role: ReadOnly, Table: "river_migration", Privilege: "SELECT"},

	// Blob store (M5, plan 6.3). studio_app, "Artifact upload: I/U
	// blob_uploads(state, sha256, size, promoted_at); I blob_objects ON
	// CONFLICT DO NOTHING" and "Artifact download: S blob_objects". The
	// column SELECT on blob_uploads is what the commit's
	// UPDATE ... WHERE id = $1 AND state = 'staging' needs to read.
	{Role: App, Table: "blob_uploads", Privilege: "INSERT"},
	{Role: App, Table: "blob_uploads", Privilege: "SELECT", Columns: []string{"id", "state", "staging_key"}},
	{Role: App, Table: "blob_uploads", Privilege: "UPDATE", Columns: []string{"state", "sha256", "size", "promoted_at"}},
	{Role: App, Table: "blob_objects", Privilege: "SELECT"},
	{Role: App, Table: "blob_objects", Privilege: "INSERT"},
	// /readyz fails while a blob health event exists (plan 11), so the API
	// reads blob_health_events.
	{Role: App, Table: "blob_health_events", Privilege: "SELECT"},
	// studio_worker, "blob.sweep: S/U blob_uploads(state); S blob_objects".
	{Role: Worker, Table: "blob_uploads", Privilege: "SELECT", Columns: []string{"id", "state", "staging_key", "expires_at"}},
	{Role: Worker, Table: "blob_uploads", Privilege: "UPDATE", Columns: []string{"state"}},
	{Role: Worker, Table: "blob_objects", Privilege: "SELECT"},
	// studio_worker, "blob.audit: S blob_objects; I blob_health_events" (X4,
	// confirmed by Godwin 2026-11-07 exactly as listed; not broadened).
	{Role: Worker, Table: "blob_health_events", Privilege: "INSERT"},
	// studio_readonly: SELECT on non-credential tables (plan 6.1).
	{Role: ReadOnly, Table: "blob_uploads", Privilege: "SELECT"},
	{Role: ReadOnly, Table: "blob_objects", Privilege: "SELECT"},
	{Role: ReadOnly, Table: "blob_health_events", Privilege: "SELECT"},
}

// riverStateInBitmask is River's one function (migration 00005).
const (
	riverStateInBitmask     = "river_job_state_in_bitmask"
	riverStateInBitmaskArgs = "bit, studio.river_job_state"
)

// ObjectGrants is the complete privilege map for sequences, functions and
// types. Every non-extension function and every enum or domain type in Schema
// is revoked from PUBLIC and the managed roles before these are applied, so
// nothing else is usable (plan 6.1: PUBLIC privileges revoked by default).
var ObjectGrants = []ObjectGrant{
	// studio_app, River insert (R5): USAGE river_job_id_seq; EXECUTE
	// river_job_state_in_bitmask; USAGE type river_job_state.
	{Role: App, Kind: Sequence, Name: "river_job_id_seq", Privilege: "USAGE"},
	{Role: App, Kind: Function, Name: riverStateInBitmask, Args: riverStateInBitmaskArgs, Privilege: "EXECUTE"},
	{Role: App, Kind: Type, Name: "river_job_state", Privilege: "USAGE"},
	// studio_worker, River client internals (R7, R8): USAGE on River
	// sequences and type; EXECUTE on River functions.
	{Role: Worker, Kind: Sequence, Name: "river_job_id_seq", Privilege: "USAGE"},
	{Role: Worker, Kind: Sequence, Name: "river_notification_id_seq", Privilege: "USAGE"},
	{Role: Worker, Kind: Function, Name: riverStateInBitmask, Args: riverStateInBitmaskArgs, Privilege: "EXECUTE"},
	{Role: Worker, Kind: Type, Name: "river_job_state", Privilege: "USAGE"},
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
		case "SELECT", "INSERT", "UPDATE", "DELETE", "MAINTAIN":
		default:
			return fmt.Errorf("grant %d: privilege %q is not allowed", i, g.Privilege)
		}
		if (g.Privilege == "DELETE" || g.Privilege == "MAINTAIN") && len(g.Columns) > 0 {
			return fmt.Errorf("grant %d: %s cannot be column-level", i, g.Privilege)
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

// ValidateObjects reports structural problems in an object grant map.
func ValidateObjects(objs []ObjectGrant) error {
	managed := map[string]bool{}
	for _, r := range ManagedRoles {
		managed[r] = true
	}
	seen := map[string]bool{}
	for i, o := range objs {
		if !managed[o.Role] {
			return fmt.Errorf("object grant %d: role %q is not a managed role", i, o.Role)
		}
		if o.Name == "" || strings.Contains(o.Name, ".") {
			return fmt.Errorf("object grant %d: name must be an unqualified name", i)
		}
		want := "USAGE"
		switch o.Kind {
		case Sequence, Type:
			if o.Args != "" {
				return fmt.Errorf("object grant %d: only functions take arguments", i)
			}
		case Function:
			want = "EXECUTE"
		default:
			return fmt.Errorf("object grant %d: kind %q is not allowed", i, o.Kind)
		}
		if o.Privilege != want {
			return fmt.Errorf("object grant %d: %s takes %s, not %q", i, o.Kind, want, o.Privilege)
		}
		k := ObjectKey(o)
		if seen[k] {
			return fmt.Errorf("object grant %d: duplicate privilege %s", i, k)
		}
		seen[k] = true
	}
	return nil
}

// ObjectKey is the normalised key of an object grant, "role|name|PRIVILEGE|".
// Sequences use the same form as tables, because the drift test reads both
// from pg_class.
func ObjectKey(o ObjectGrant) string {
	return fmt.Sprintf("%s|%s|%s|", o.Role, o.Name, o.Privilege)
}

// ExpectedObjectKeys returns the sorted keys of the object grants of kind.
func ExpectedObjectKeys(objs []ObjectGrant, kind ObjectKind) []string {
	out := []string{}
	for _, o := range objs {
		if o.Kind == kind {
			out = append(out, ObjectKey(o))
		}
	}
	sort.Strings(out)
	return out
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

// Statements returns the SQL that resets and applies the privilege maps. Run
// it in one transaction as the schema owner.
func Statements(grants []Grant, objs []ObjectGrant) []string {
	revokeFrom := "PUBLIC"
	for _, r := range ManagedRoles {
		revokeFrom += ", " + ident(r)
	}
	stmts := []string{
		// Table-level REVOKE also revokes column-level privileges.
		fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA %s FROM %s", ident(Schema), revokeFrom),
		fmt.Sprintf("REVOKE ALL ON ALL SEQUENCES IN SCHEMA %s FROM %s", ident(Schema), revokeFrom),
		fmt.Sprintf("REVOKE ALL ON SCHEMA %s FROM %s", ident(Schema), revokeFrom),
		// Our own functions (not extension members) are callable only as
		// ObjectGrants says. Extension functions (citext) keep their
		// defaults because the citext operators depend on them.
		fmt.Sprintf(`DO $$
DECLARE f regprocedure;
BEGIN
    FOR f IN
        SELECT p.oid::regprocedure FROM pg_proc p
        WHERE p.pronamespace = %s::regnamespace
          AND NOT EXISTS (SELECT 1 FROM pg_depend d
                          WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
    LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %%s FROM %s', f);
    END LOOP;
END $$`, quoteLiteral(Schema), strings.ReplaceAll(revokeFrom, "%", "%%")),
		// Our own enum and domain types (not extension members, not table row
		// types or arrays) are usable only as ObjectGrants says.
		fmt.Sprintf(`DO $$
DECLARE t regtype;
BEGIN
    FOR t IN
        SELECT ty.oid::regtype FROM pg_type ty
        WHERE ty.typnamespace = %s::regnamespace
          AND ty.typtype IN ('e', 'd')
          AND NOT EXISTS (SELECT 1 FROM pg_depend d
                          WHERE d.classid = 'pg_type'::regclass AND d.objid = ty.oid AND d.deptype = 'e')
    LOOP
        EXECUTE format('REVOKE ALL ON TYPE %%s FROM %s', t);
    END LOOP;
END $$`, quoteLiteral(Schema), strings.ReplaceAll(revokeFrom, "%", "%%")),
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
	for _, o := range objs {
		target := ident(Schema) + "." + ident(o.Name)
		if o.Kind == Function {
			target += "(" + o.Args + ")"
		}
		stmts = append(stmts, fmt.Sprintf("GRANT %s ON %s %s TO %s", o.Privilege, o.Kind, target, ident(o.Role)))
	}
	return stmts
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
