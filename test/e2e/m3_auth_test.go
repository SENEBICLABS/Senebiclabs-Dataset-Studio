//go:build integration

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

func do(t *testing.T, method, url, token string, body any) (int, http.Header, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, url, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

// withoutRequestID drops fields that differ per request.
func withoutRequestID(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	delete(m, "request_id")
	return m
}

func TestAuthenticationOverHTTP(t *testing.T) {
	db := testdb.New(t)

	// The first administrator exists only through studioctl (plan 14); the
	// password comes from stdin.
	ctl := exec.Command(filepath.Join(requireEnv(t, "STUDIO_BIN_DIR"), "studioctl"), //nolint:noctx // short-lived
		"create-admin", "--email", "Admin@Example.test", "--name", "E2E Admin")
	ctl.Env = []string{"PATH=" + os.Getenv("PATH"), "STUDIO_DATABASE_URL=" + db.URL(dbroles.App)}
	ctl.Stdin = strings.NewReader("e2e-admin-password\n")
	if out, err := ctl.CombinedOutput(); err != nil || !strings.Contains(string(out), "created platform administrator admin@example.test") {
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

	code, hdr, _ := do(t, http.MethodGet, base+"/v1/me", "", nil)
	if code != http.StatusUnauthorized || !strings.HasPrefix(hdr.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("unauthenticated /v1/me = %d %q", code, hdr.Get("WWW-Authenticate"))
	}

	// Uniform failures: wrong password and unknown user are indistinguishable.
	c1, _, b1 := do(t, http.MethodPost, base+"/v1/auth/login", "", map[string]string{"email": "admin@example.test", "password": "wrong-password-1"})
	c2, _, b2 := do(t, http.MethodPost, base+"/v1/auth/login", "", map[string]string{"email": "ghost@example.test", "password": "wrong-password-1"})
	m1, m2 := withoutRequestID(t, b1), withoutRequestID(t, b2)
	if c1 != http.StatusUnauthorized || c2 != http.StatusUnauthorized || !mapsEqual(m1, m2) {
		t.Errorf("failures differ:\n%d %v\n%d %v", c1, m1, c2, m2)
	}

	//nolint:gosec // G101: synthetic test password for a throwaway test database; the test asserts it never reaches the logs.
	code, _, body := do(t, http.MethodPost, base+"/v1/auth/login", "", map[string]string{"email": "admin@example.test", "password": "e2e-admin-password"})
	var login struct {
		Token, TokenType, ExpiresAt, PrincipalID string
	}
	_ = json.Unmarshal(body, &login)
	if code != http.StatusOK || login.Token == "" {
		t.Fatalf("login = %d %s", code, body)
	}

	code, _, body = do(t, http.MethodGet, base+"/v1/me", login.Token, nil)
	var me map[string]string
	_ = json.Unmarshal(body, &me)
	if code != http.StatusOK || me["kind"] != "user" || me["auth_method"] != "session" || me["principal_id"] == "" {
		t.Errorf("/v1/me = %d %s", code, body)
	}

	if code, _, body = do(t, http.MethodPost, base+"/v1/auth/logout", login.Token, nil); code != http.StatusNoContent {
		t.Errorf("logout = %d %s", code, body)
	}
	if code, _, _ = do(t, http.MethodGet, base+"/v1/me", login.Token, nil); code != http.StatusUnauthorized {
		t.Errorf("/v1/me after logout = %d", code)
	}

	if exit := p.stop(t, 15*time.Second); exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	logs := p.logs.String()
	for _, secret := range []string{login.Token, "e2e-admin-password", "wrong-password-1", e2eHMACKey} {
		if strings.Contains(logs, secret) {
			t.Error("a secret appears in the API logs")
		}
	}

	// The failed attempts were flushed on shutdown (AG1) as one event each.
	admin := db.Connect(t, testdb.Admin)
	var n int
	if err := admin.QueryRow(context.Background(),
		`SELECT count(*) FROM studio.audit_events WHERE action = 'auth.login_failed'`).Scan(&n); err != nil || n != 2 {
		t.Errorf("auth.login_failed events after shutdown = %d (%v), want 2", n, err)
	}
}

func mapsEqual(a, b map[string]any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}
