//go:build integration

package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// User administration end to end: authorship, validation, role changes that
// take effect at once, password reset ending sessions, and disabling.
func TestUserAdministrationLifecycle(t *testing.T) {
	e := newAPIEnv(t)
	rootID, root := e.bootstrapAdmin(t)

	userID, email := e.createUser(t, root, "member")
	// Plan 7.2: created_by and the audit actor are the authenticated caller.
	var createdBy, actor, principalAgrees string
	if err := e.admin.QueryRow(ctx(t), `
		SELECT u.created_by::text, (p.created_by = u.created_by)::text,
		       (SELECT actor_principal_id::text FROM studio.audit_events
		         WHERE action = 'user.created' AND target_id = $1)
		FROM studio.users u JOIN studio.principals p ON p.id = u.principal_id
		WHERE u.principal_id = $1::uuid`, userID.String()).Scan(&createdBy, &principalAgrees, &actor); err != nil {
		t.Fatal(err)
	}
	if createdBy != rootID.String() || principalAgrees != "true" || actor != rootID.String() {
		t.Errorf("created_by %s (principal agrees: %s), audit actor %s; want %s", createdBy, principalAgrees, actor, rootID)
	}

	for name, body := range map[string]map[string]string{
		"duplicate email": {"email": strings.ToUpper(email), "display_name": "Dup", "password": userPassword},
		"unknown role":    {"email": "x@example.test", "display_name": "X", "password": userPassword, "global_role": "root"},
		"short password":  {"email": "y@example.test", "display_name": "Y", "password": "short"},
		"bad email":       {"email": "not-an-email", "display_name": "Z", "password": userPassword},
	} {
		r := e.call(t, http.MethodPost, "/v1/users", root, body)
		want := http.StatusBadRequest
		if name == "duplicate email" {
			want = http.StatusConflict
		}
		if r.code != want {
			t.Errorf("%s = %d %s, want %d", name, r.code, r.body, want)
		}
	}
	if r := e.call(t, http.MethodPost, "/v1/users", root, map[string]any{"email": "q@example.test", "admin": true}); r.code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", r.code)
	}

	// A role change takes effect on the next request with the same token.
	tok := e.login(t, email, userPassword)
	if r := e.call(t, http.MethodGet, "/v1/users", tok, nil); r.code != http.StatusForbidden {
		t.Fatalf("member listing users = %d", r.code)
	}
	if r := e.call(t, http.MethodPatch, "/v1/users/"+userID.String(), root, map[string]string{"global_role": "platform_admin"}); r.code != http.StatusOK {
		t.Fatalf("promote = %d %s", r.code, r.body)
	}
	if r := e.call(t, http.MethodGet, "/v1/users", tok, nil); r.code != http.StatusOK {
		t.Errorf("promoted user listing users = %d", r.code)
	}
	if me := e.call(t, http.MethodGet, "/v1/me", tok, nil).json(t); me["global_role"] != "platform_admin" {
		t.Errorf("/v1/me global_role = %v", me["global_role"])
	}
	if r := e.call(t, http.MethodPatch, "/v1/users/"+userID.String(), root, map[string]string{}); r.code != http.StatusBadRequest {
		t.Errorf("empty patch = %d, want 400", r.code)
	}
	if r := e.call(t, http.MethodPatch, "/v1/users/"+userID.String(), root, map[string]string{"email": "new@example.test"}); r.code != http.StatusBadRequest {
		t.Errorf("email change = %d, want 400 (email is not updatable)", r.code)
	}

	// A password reset revokes open sessions; the event never carries the
	// password or its hash.
	newPassword := "a-brand-new-password"
	r := e.call(t, http.MethodPatch, "/v1/users/"+userID.String(), root, map[string]string{"password": newPassword})
	if r.code != http.StatusOK || strings.Contains(string(r.body), "argon2") {
		t.Fatalf("password reset = %d %s", r.code, r.body)
	}
	if r := e.call(t, http.MethodGet, "/v1/me", tok, nil); r.code != http.StatusUnauthorized {
		t.Errorf("old session after password reset = %d, want 401", r.code)
	}
	var fields, revoked string
	if err := e.admin.QueryRow(ctx(t), `SELECT details->>'fields', details->>'sessions_revoked'
		FROM studio.audit_events WHERE request_id = $1`, r.requestID).Scan(&fields, &revoked); err != nil {
		t.Fatal(err)
	}
	if fields != `["password"]` || revoked != "1" {
		t.Errorf("user.updated details fields=%s sessions_revoked=%s", fields, revoked)
	}
	if n := countEvents(t, e.admin, `details::text LIKE '%argon2%' OR details::text LIKE '%' || $1 || '%'`, newPassword); n != 0 {
		t.Error("a password or hash reached the audit log")
	}
	tok = e.login(t, email, newPassword)

	// The user's API key and session stop working when the user is disabled.
	key, _ := e.issueKey(t, root, userID)
	if r := e.call(t, http.MethodGet, "/v1/me", key, nil); r.code != http.StatusOK {
		t.Fatalf("user key = %d", r.code)
	}
	if r := e.call(t, http.MethodPost, "/v1/users/"+userID.String()+":disable", root, nil); r.code != http.StatusOK ||
		r.json(t)["status"] != "disabled" {
		t.Fatalf("disable = %d %s", r.code, r.body)
	}
	for name, cred := range map[string]string{"session": tok, "api key": key} {
		if r := e.call(t, http.MethodGet, "/v1/me", cred, nil); r.code != http.StatusUnauthorized {
			t.Errorf("%s of a disabled user = %d, want 401", name, r.code)
		}
	}
	if r := e.call(t, http.MethodPost, "/v1/auth/login", "", map[string]string{"email": email, "password": newPassword}); r.code != http.StatusUnauthorized {
		t.Errorf("disabled user login = %d", r.code)
	}
	if r := e.call(t, http.MethodPost, "/v1/users/"+userID.String()+":disable", root, nil); r.code != http.StatusConflict {
		t.Errorf("second disable = %d, want 409", r.code)
	}

	// Routing edge cases, as an administrator: a malformed id names nothing,
	// and an unknown custom method is not a route.
	for _, path := range []string{"/v1/users/not-a-uuid", "/v1/users/" + userID.String() + ":zap"} {
		if r := e.call(t, http.MethodGet, path, root, nil); r.code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, r.code)
		}
	}
	if r := e.call(t, http.MethodPost, "/v1/users/"+userID.String()+":enable", root, nil); r.code != http.StatusNotFound {
		t.Errorf("unknown custom method = %d, want 404", r.code)
	}
}

// Service accounts authenticate only with API keys; keys are listed without
// secrets and revoked permanently.
func TestServiceAccountsAndKeys(t *testing.T) {
	e := newAPIEnv(t)
	rootID, root := e.bootstrapAdmin(t)

	r := e.call(t, http.MethodPost, "/v1/service-accounts", root, map[string]string{"name": "loader", "description": "nightly loader"})
	if r.code != http.StatusCreated {
		t.Fatalf("create = %d %s", r.code, r.body)
	}
	sa := r.json(t)
	if sa["name"] != "loader" || sa["global_role"] != "member" {
		t.Errorf("created %v", sa)
	}
	if r := e.call(t, http.MethodPost, "/v1/service-accounts", root, map[string]string{"name": "loader"}); r.code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", r.code)
	}
	if r := e.call(t, http.MethodPost, "/v1/service-accounts", root, map[string]string{"name": "Bad Name!"}); r.code != http.StatusBadRequest {
		t.Errorf("invalid name = %d, want 400", r.code)
	}
	saID := sa["id"].(string)

	if r := e.call(t, http.MethodPost, "/v1/principals/"+saID+"/api-keys", root, map[string]string{"expires_at": "tomorrow"}); r.code != http.StatusBadRequest {
		t.Errorf("malformed expiry = %d, want 400", r.code)
	}
	if r := e.call(t, http.MethodPost, "/v1/principals/"+saID+"/api-keys", root,
		map[string]string{"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}); r.code != http.StatusBadRequest {
		t.Errorf("past expiry = %d, want 400", r.code)
	}
	if r := e.call(t, http.MethodPost, "/v1/principals/00000000-0000-7000-8000-000000000003/api-keys", root,
		map[string]string{"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}); r.code != http.StatusBadRequest {
		t.Errorf("key for system:worker = %d, want 400", r.code)
	}

	key, keyID := e.issueKey(t, root, mustUUID(t, saID))
	me := e.call(t, http.MethodGet, "/v1/me", key, nil).json(t)
	if me["kind"] != "service_account" || me["auth_method"] != "api_key" || me["principal_id"] != saID {
		t.Errorf("/v1/me with key = %v", me)
	}
	if r := e.call(t, http.MethodPost, "/v1/auth/logout", key, nil); r.code != http.StatusBadRequest {
		t.Errorf("logout with an API key = %d, want 400", r.code)
	}

	list := e.call(t, http.MethodGet, "/v1/principals/"+saID+"/api-keys", root, nil)
	secret := key[strings.LastIndex(key, "_")+1:]
	if list.code != http.StatusOK || strings.Contains(string(list.body), secret) || strings.Contains(string(list.body), "hash") ||
		!strings.Contains(string(list.body), keyID.String()) || !strings.Contains(string(list.body), rootID.String()) {
		t.Errorf("key list = %d %s", list.code, list.body)
	}

	if r := e.call(t, http.MethodDelete, "/v1/api-keys/"+keyID.String(), root, nil); r.code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", r.code, r.body)
	}
	if r := e.call(t, http.MethodGet, "/v1/me", key, nil); r.code != http.StatusUnauthorized {
		t.Errorf("revoked key = %d, want 401", r.code)
	}
	if r := e.call(t, http.MethodDelete, "/v1/api-keys/"+keyID.String(), root, nil); r.code != http.StatusNotFound {
		t.Errorf("second revoke = %d, want 404", r.code)
	}
	if r := e.call(t, http.MethodGet, "/v1/principals/"+mustUUID(t, "0190a6e4-0000-7000-8000-00000000dead").String()+"/api-keys", root, nil); r.code != http.StatusNotFound {
		t.Errorf("keys of an unknown principal = %d, want 404", r.code)
	}
}

// Cursor pagination returns every row exactly once.
func TestListPagination(t *testing.T) {
	e := newAPIEnv(t)
	_, root := e.bootstrapAdmin(t)
	for range 4 {
		e.createUser(t, root, "member")
	}
	seen := map[string]int{}
	cursor, pages := "", 0
	for {
		r := e.call(t, http.MethodGet, "/v1/users?limit=2&cursor="+cursor, root, nil)
		if r.code != http.StatusOK {
			t.Fatalf("page = %d %s", r.code, r.body)
		}
		m := r.json(t)
		for _, it := range m["items"].([]any) {
			seen[it.(map[string]any)["id"].(string)]++
		}
		pages++
		next, _ := m["next_cursor"].(string)
		if next == "" || pages > 10 {
			break
		}
		cursor = next
	}
	if len(seen) != 5 {
		t.Errorf("saw %d users across %d pages, want 5 (admin + 4)", len(seen), pages)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("user %s listed %d times", id, n)
		}
	}
	for _, q := range []string{"limit=0", "limit=201", "limit=x", "cursor=!!"} {
		if r := e.call(t, http.MethodGet, "/v1/users?"+q, root, nil); r.code != http.StatusBadRequest {
			t.Errorf("?%s = %d, want 400", q, r.code)
		}
	}
	if r := e.call(t, http.MethodGet, "/v1/service-accounts?limit=1", root, nil); r.code != http.StatusOK {
		t.Errorf("service-account list = %d", r.code)
	}
}
