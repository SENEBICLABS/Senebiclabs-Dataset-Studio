//go:build integration

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/testdb"
)

// M4 outcome "403s are audited", through the real binaries: an administrator
// created by studioctl creates a member over HTTP; the member's refused
// requests are 403 and reach audit_events as one aggregated denied event when
// the API shuts down.
func TestAuthorizationOverHTTP(t *testing.T) {
	db := testdb.New(t)
	ctl := exec.Command(filepath.Join(requireEnv(t, "STUDIO_BIN_DIR"), "studioctl"), //nolint:noctx // short-lived
		"create-admin", "--email", "admin@example.test", "--name", "E2E Admin")
	ctl.Env = []string{"PATH=" + os.Getenv("PATH"), "STUDIO_DATABASE_URL=" + db.URL(dbroles.App)}
	ctl.Stdin = strings.NewReader("e2e-admin-password\n")
	if out, err := ctl.CombinedOutput(); err != nil {
		t.Fatalf("create-admin: %v\n%s", err, out)
	}

	addr := freeAddr(t)
	base := "http://" + addr
	p := start(t, "studio-api",
		"STUDIO_HTTP_ADDR="+addr,
		"STUDIO_DATABASE_URL="+db.URL(dbroles.App),
		"STUDIO_AUDIT_HMAC_KEY="+e2eHMACKey,
	)
	waitHealthy(t, base)

	login := func(email, password string) string {
		code, _, body := do(t, http.MethodPost, base+"/v1/auth/login", "", map[string]string{"email": email, "password": password})
		var r struct{ Token string }
		_ = json.Unmarshal(body, &r)
		if code != http.StatusOK || r.Token == "" {
			t.Fatalf("login %s = %d %s", email, code, body)
		}
		return r.Token
	}
	admin := login("admin@example.test", "e2e-admin-password")

	//nolint:gosec // G101: synthetic test password for a throwaway test database.
	memberPassword := "e2e-member-password"
	code, _, body := do(t, http.MethodPost, base+"/v1/users", admin, map[string]string{
		"email": "member@example.test", "display_name": "Member", "password": memberPassword,
	})
	if code != http.StatusCreated {
		t.Fatalf("create member = %d %s", code, body)
	}
	member := login("member@example.test", memberPassword)

	for range 3 {
		if code, _, body := do(t, http.MethodGet, base+"/v1/users", member, nil); code != http.StatusForbidden ||
			!strings.Contains(string(body), "permission denied") {
			t.Fatalf("member GET /v1/users = %d %s", code, body)
		}
	}
	if code, _, _ := do(t, http.MethodGet, base+"/v1/users", admin, nil); code != http.StatusOK {
		t.Errorf("admin GET /v1/users = %d", code)
	}

	if exit := p.stop(t, 15*time.Second); exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	if logs := p.logs.String(); strings.Contains(logs, memberPassword) || strings.Contains(logs, member) {
		t.Error("a secret appears in the API logs")
	}

	conn := db.Connect(t, testdb.Admin)
	var n, repeats int
	var actorEmail, perm string
	if err := conn.QueryRow(context.Background(), `
		SELECT count(*), coalesce(sum((details->>'repeat_count')::int), 0)::int,
		       coalesce(min(u.email::text), ''), coalesce(min(details->>'permission'), '')
		FROM studio.audit_events a JOIN studio.users u ON u.principal_id = a.actor_principal_id
		WHERE a.outcome = 'denied' AND a.action = 'user.list'`).Scan(&n, &repeats, &actorEmail, &perm); err != nil {
		t.Fatal(err)
	}
	if n != 1 || repeats != 3 || actorEmail != "member@example.test" || perm != "manage_users" {
		t.Errorf("denied events: %d rows, repeat_count %d, actor %q, permission %q; want 1, 3, member, manage_users",
			n, repeats, actorEmail, perm)
	}
}
