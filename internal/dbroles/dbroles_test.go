package dbroles

import (
	"slices"
	"strings"
	"testing"
)

func TestGrantMapIsValid(t *testing.T) {
	if err := Validate(Grants); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsBadGrants(t *testing.T) {
	cases := map[string][]Grant{
		"unmanaged role":        {{Role: Owner, Table: "principals", Privilege: "SELECT"}},
		"public":                {{Role: "PUBLIC", Table: "principals", Privilege: "SELECT"}},
		"truncate":              {{Role: App, Table: "principals", Privilege: "TRUNCATE"}},
		"references":            {{Role: App, Table: "principals", Privilege: "REFERENCES"}},
		"column delete":         {{Role: App, Table: "principals", Privilege: "DELETE", Columns: []string{"id"}}},
		"qualified table":       {{Role: App, Table: "studio.principals", Privilege: "SELECT"}},
		"duplicate table-level": {{Role: App, Table: "t", Privilege: "SELECT"}, {Role: App, Table: "t", Privilege: "SELECT"}},
		"duplicate column":      {{Role: App, Table: "t", Privilege: "SELECT", Columns: []string{"a", "a"}}},
	}
	for name, g := range cases {
		if err := Validate(g); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestStatementsAreDeterministicAndRevokeFirst(t *testing.T) {
	a, b := Statements(Grants), Statements(Grants)
	if !slices.Equal(a, b) {
		t.Fatal("Statements is not deterministic")
	}
	firstGrant := slices.IndexFunc(a, func(s string) bool { return strings.HasPrefix(s, "GRANT") })
	lastRevoke := -1
	for i, s := range a {
		if strings.HasPrefix(s, "REVOKE") || strings.HasPrefix(s, "DO $$") {
			lastRevoke = i
		}
	}
	if firstGrant < 0 || lastRevoke > firstGrant {
		t.Fatalf("revokes must all precede grants: %v", a)
	}
	if !strings.Contains(a[0], "FROM PUBLIC") {
		t.Errorf("first revoke must include PUBLIC: %s", a[0])
	}
}

func TestStatementsQuoteIdentifiers(t *testing.T) {
	s := Statements([]Grant{{Role: App, Table: `we"ird`, Privilege: "SELECT", Columns: []string{"c"}}})
	last := s[len(s)-1]
	if last != `GRANT SELECT ("c") ON "studio"."we""ird" TO "studio_app"` {
		t.Errorf("unexpected statement: %s", last)
	}
}

func TestNoWorkerOrReadOnlyAccessToCredentials(t *testing.T) {
	for _, g := range Grants {
		switch {
		case (g.Role == Worker || g.Role == ReadOnly) && (g.Table == "sessions" || g.Table == "api_keys"):
			t.Errorf("%s must not access credential table %s: %+v", g.Role, g.Table, g)
		case g.Role == Worker && (g.Table == "users" || g.Table == "service_accounts" || g.Table == "principals"):
			t.Errorf("worker must not access %s: %+v", g.Table, g)
		case g.Role == Worker && g.Table == "idempotency_keys" && g.Privilege == "SELECT" &&
			!slices.Equal(g.Columns, []string{"expires_at"}):
			t.Errorf("worker may only read idempotency_keys.expires_at: %+v", g)
		case g.Role == ReadOnly && g.Table == "idempotency_keys":
			t.Errorf("readonly must not read idempotency_keys: %+v", g)
		case g.Role == ReadOnly && g.Table == "users" && (len(g.Columns) == 0 || slices.Contains(g.Columns, "password_hash")):
			t.Errorf("readonly must not read users.password_hash: %+v", g)
		case g.Role == Backup || g.Role == Migrator:
			t.Errorf("%s must hold no table grants: %+v", g.Role, g)
		}
	}
}

func TestExpectedKeysExpandColumns(t *testing.T) {
	got := ExpectedKeys([]Grant{
		{Role: App, Table: "t", Privilege: "SELECT", Columns: []string{"b", "a"}},
		{Role: Worker, Table: "t", Privilege: "DELETE"},
	})
	want := []string{"studio_app|t|SELECT|a", "studio_app|t|SELECT|b", "studio_worker|t|DELETE|"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
