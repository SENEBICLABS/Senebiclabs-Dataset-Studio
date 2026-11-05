//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/api"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/auth"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/health"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/httpx"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/testdb"
)

// apiEnv runs the real studio-api handler (internal/api) in process, against
// a cloned database, connected as studio_app (G1).
type apiEnv struct {
	db     *testdb.DB
	admin  *pgx.Conn // superuser, for inspection only
	agg    *audit.Aggregator
	srv    *httptest.Server
	routes []httpx.Route
	seq    atomic.Int64
}

func newAPIEnv(t *testing.T) *apiEnv {
	t.Helper()
	db := testdb.New(t)
	pool := appPool(t, db)
	log := slog.New(slog.DiscardHandler)
	// A long window: tests flush explicitly and then inspect.
	agg := audit.NewAggregator(pool, log, audit.Actions, audit.AggregatorConfig{Window: time.Hour})
	routes := append(health.New(log, time.Second).Routes(),
		api.Routes(pool, agg, log, api.Config{HMACKey: testHMACKey, IdleTimeout: 30 * time.Minute})...)
	h, err := api.Handler(log, routes)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &apiEnv{db: db, admin: db.Connect(t, testdb.Admin), agg: agg, srv: srv, routes: routes}
}

type response struct {
	code      int
	requestID string
	body      []byte
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.code, r.body)
	}
	return m
}

func (e *apiEnv) call(t *testing.T, method, path, token string, body any) response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx(t), method, e.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return response{code: resp.StatusCode, requestID: resp.Header.Get(httpx.RequestIDHeader), body: b}
}

func (e *apiEnv) unique(prefix string) string { return fmt.Sprintf("%s%d", prefix, e.seq.Add(1)) }

// bootstrapAdmin creates the first platform administrator the only way the
// plan allows (studioctl's service method, as system:cli) and logs in.
func (e *apiEnv) bootstrapAdmin(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	svc := auth.NewService(appPool(t, e.db), auth.Config{HMACKey: testHMACKey, Failures: e.agg})
	id, err := svc.CreateAdmin(ctx(t), auth.AdminInput{Email: "root-admin@example.test", DisplayName: "Root Admin",
		Password: adminPassword, OSUser: "tester", Hostname: "test-host"})
	if err != nil {
		t.Fatal(err)
	}
	return id, e.login(t, "root-admin@example.test", adminPassword)
}

func (e *apiEnv) login(t *testing.T, email, password string) string {
	t.Helper()
	r := e.call(t, http.MethodPost, "/v1/auth/login", "", map[string]string{"email": email, "password": password})
	if r.code != http.StatusOK {
		t.Fatalf("login %s = %d %s", email, r.code, r.body)
	}
	return r.json(t)["token"].(string)
}

const userPassword = "user-password-for-tests"

func (e *apiEnv) createUser(t *testing.T, adminToken, role string) (uuid.UUID, string) {
	t.Helper()
	email := e.unique("user") + "@example.test"
	r := e.call(t, http.MethodPost, "/v1/users", adminToken, map[string]string{
		"email": email, "display_name": "User " + email, "password": userPassword, "global_role": role,
	})
	if r.code != http.StatusCreated {
		t.Fatalf("create user = %d %s", r.code, r.body)
	}
	return uuid.MustParse(r.json(t)["id"].(string)), email
}

func (e *apiEnv) createServiceAccount(t *testing.T, adminToken, role string) uuid.UUID {
	t.Helper()
	r := e.call(t, http.MethodPost, "/v1/service-accounts", adminToken, map[string]string{
		"name": e.unique("svc-"), "description": "test", "global_role": role,
	})
	if r.code != http.StatusCreated {
		t.Fatalf("create service account = %d %s", r.code, r.body)
	}
	return uuid.MustParse(r.json(t)["id"].(string))
}

func (e *apiEnv) issueKey(t *testing.T, adminToken string, principal uuid.UUID) (string, uuid.UUID) {
	t.Helper()
	r := e.call(t, http.MethodPost, "/v1/principals/"+principal.String()+"/api-keys", adminToken,
		map[string]string{"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if r.code != http.StatusCreated {
		t.Fatalf("issue key = %d %s", r.code, r.body)
	}
	m := r.json(t)
	return m["key"].(string), uuid.MustParse(m["id"].(string))
}

// caller is one column of the route × role matrix.
type caller struct {
	name     string
	id       uuid.UUID
	token    string // empty for anonymous
	session  bool
	email    string // users only: for the login probe
	newToken func(t *testing.T) string
}

// access is what a route requires, written from plan 14 and 15 rather than
// read from the code, so the test is an independent statement of the matrix.
type access int

const (
	public        access = iota // no authentication
	authenticated               // any authenticated principal, acting on itself
	manageUsers                 // plan 14 "Manage users and service accounts": platform_admin only
)

// expectedAccess lists every route the API serves. A route missing here, or
// listed here but not served, fails TestRouteRoleMatrix (plan 17.3 #1).
var expectedAccess = map[string]access{
	"GET /healthz":                      public,
	"GET /readyz":                       public,
	"POST /v1/auth/login":               public,
	"POST /v1/auth/logout":              authenticated,
	"GET /v1/me":                        authenticated,
	"GET /v1/users":                     manageUsers,
	"POST /v1/users":                    manageUsers,
	"GET /v1/users/{id}":                manageUsers,
	"PATCH /v1/users/{id}":              manageUsers,
	"POST /v1/users/{id}:disable":       manageUsers,
	"GET /v1/service-accounts":          manageUsers,
	"POST /v1/service-accounts":         manageUsers,
	"POST /v1/principals/{id}/api-keys": manageUsers,
	"GET /v1/principals/{id}/api-keys":  manageUsers,
	"DELETE /v1/api-keys/{id}":          manageUsers,
}

// deniedAction is the action a refused request is recorded under.
var deniedAction = map[string]string{
	"GET /v1/users":                     audit.ActionUserList,
	"POST /v1/users":                    audit.ActionUserCreated,
	"GET /v1/users/{id}":                audit.ActionUserRead,
	"PATCH /v1/users/{id}":              audit.ActionUserUpdated,
	"POST /v1/users/{id}:disable":       audit.ActionUserDisabled,
	"GET /v1/service-accounts":          audit.ActionServiceAccountList,
	"POST /v1/service-accounts":         audit.ActionServiceAccountCreated,
	"POST /v1/principals/{id}/api-keys": audit.ActionAPIKeyCreated,
	"GET /v1/principals/{id}/api-keys":  audit.ActionAPIKeyList,
	"DELETE /v1/api-keys/{id}":          audit.ActionAPIKeyRevoked,
}

func mutating(method string) bool { return method != http.MethodGet }

// TestRouteRoleMatrix is the route × role matrix test (plan 14, 17.3 #1;
// M4 outcome "the matrix is complete, and 403s are audited"). For every
// served route and every kind of caller it asserts:
//
//   - anonymous callers get 401 on protected routes and leave no audit event;
//   - callers without the permission get 403, before any lookup (a random id
//     still gives 403), and each refusal becomes a denied audit event;
//   - permitted callers are neither 401 nor 403, and every mutating request
//     that succeeds writes exactly one succeeded event, carrying the request
//     id, whose actor is the caller (plan 8.1, 7.2).
func TestRouteRoleMatrix(t *testing.T) {
	e := newAPIEnv(t)

	served := map[string]bool{}
	for _, rt := range e.routes {
		served[rt.String()] = true
		if _, ok := expectedAccess[rt.String()]; !ok {
			t.Errorf("route %s is served but has no row in the matrix", rt)
		}
	}
	for r := range expectedAccess {
		if !served[r] {
			t.Errorf("matrix row %s is not served", r)
		}
	}

	rootID, rootToken := e.bootstrapAdmin(t)

	userCaller := func(name, role string) caller {
		id, email := e.createUser(t, rootToken, role)
		c := caller{name: name, id: id, session: true, email: email}
		c.newToken = func(t *testing.T) string { return e.login(t, email, userPassword) }
		c.token = c.newToken(t)
		return c
	}
	saCaller := func(name, role string) caller {
		id := e.createServiceAccount(t, rootToken, role)
		c := caller{name: name, id: id}
		c.newToken = func(t *testing.T) string { k, _ := e.issueKey(t, rootToken, id); return k }
		c.token = c.newToken(t)
		return c
	}
	callers := []caller{
		{name: "anonymous"},
		userCaller("user/member", "member"),
		userCaller("user/registry_admin", "registry_admin"),
		userCaller("user/platform_admin", "platform_admin"),
		saCaller("service_account/member", "member"),
		saCaller("service_account/registry_admin", "registry_admin"),
		saCaller("service_account/platform_admin", "platform_admin"),
	}
	allowed := func(c caller, a access) bool {
		switch a {
		case public:
			return true
		case authenticated:
			return c.token != ""
		default:
			return strings.HasSuffix(c.name, "/platform_admin")
		}
	}

	// Each request gets a fresh, valid target so permitted calls succeed.
	type req struct {
		path  string
		ghost string // the same route with an id that names nothing
		body  any
		token string
	}
	build := func(t *testing.T, route string, c caller) req {
		r := req{token: c.token}
		// at sets path for a fresh target and ghost for a random id.
		at := func(format string, id uuid.UUID) {
			r.path, r.ghost = fmt.Sprintf(format, id), fmt.Sprintf(format, uuid.New())
		}
		target := func() uuid.UUID { id, _ := e.createUser(t, rootToken, "member"); return id }
		switch route {
		case "GET /healthz":
			r.path = "/healthz"
		case "GET /readyz":
			r.path = "/readyz"
		case "POST /v1/auth/login":
			_, email := e.createUser(t, rootToken, "member")
			r.path, r.body = "/v1/auth/login", map[string]string{"email": email, "password": userPassword}
		case "POST /v1/auth/logout":
			// A separate credential, so the caller's own token survives.
			r.path = "/v1/auth/logout"
			if c.newToken != nil {
				r.token = c.newToken(t)
			}
		case "GET /v1/me":
			r.path = "/v1/me"
		case "GET /v1/users":
			r.path = "/v1/users"
		case "POST /v1/users":
			r.path = "/v1/users"
			r.body = map[string]string{"email": e.unique("new") + "@example.test", "display_name": "New",
				"password": userPassword, "global_role": "member"}
		case "GET /v1/users/{id}":
			at("/v1/users/%s", target())
		case "PATCH /v1/users/{id}":
			at("/v1/users/%s", target())
			r.body = map[string]string{"display_name": "Renamed"}
		case "POST /v1/users/{id}:disable":
			at("/v1/users/%s:disable", target())
		case "GET /v1/service-accounts":
			r.path = "/v1/service-accounts"
		case "POST /v1/service-accounts":
			r.path, r.body = "/v1/service-accounts", map[string]string{"name": e.unique("sa-"), "global_role": "member"}
		case "POST /v1/principals/{id}/api-keys":
			at("/v1/principals/%s/api-keys", target())
			r.body = map[string]string{"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
		case "GET /v1/principals/{id}/api-keys":
			at("/v1/principals/%s/api-keys", target())
		case "DELETE /v1/api-keys/{id}":
			_, keyID := e.issueKey(t, rootToken, target())
			at("/v1/api-keys/%s", keyID)
		default:
			t.Fatalf("no request builder for %s", route)
		}
		return r
	}

	type denial struct{ actor, action string }
	wantDenied := map[denial]int{}
	routes := make([]string, 0, len(expectedAccess))
	for r := range expectedAccess {
		routes = append(routes, r)
	}
	sort.Strings(routes)

	for _, route := range routes {
		method, _, _ := strings.Cut(route, " ")
		a := expectedAccess[route]
		for _, c := range callers {
			t.Run(route+"/"+c.name, func(t *testing.T) {
				r := build(t, route, c)
				res := e.call(t, method, r.path, r.token, r.body)
				switch {
				case a != public && c.token == "":
					if res.code != http.StatusUnauthorized {
						t.Fatalf("anonymous = %d %s, want 401", res.code, res.body)
					}
					if n := countEvents(t, e.admin, `request_id = $1`, res.requestID); n != 0 {
						t.Errorf("an unauthenticated request was audited (%d events)", n)
					}
				case !allowed(c, a):
					if res.code != http.StatusForbidden {
						t.Fatalf("%s = %d %s, want 403", c.name, res.code, res.body)
					}
					wantDenied[denial{c.id.String(), deniedAction[route]}]++
					// Denial comes before lookup: an id that names nothing is 403 too.
					if r.ghost != "" {
						if g := e.call(t, method, r.ghost, r.token, r.body); g.code != http.StatusForbidden {
							t.Errorf("unknown id %s = %d, want 403", r.ghost, g.code)
						} else {
							wantDenied[denial{c.id.String(), deniedAction[route]}]++
						}
					}
				default:
					if res.code == http.StatusUnauthorized || res.code == http.StatusForbidden {
						t.Fatalf("%s = %d %s, want access", c.name, res.code, res.body)
					}
					if res.code >= 500 {
						t.Fatalf("%s = %d %s", c.name, res.code, res.body)
					}
					if mutating(method) && res.code < 300 && route != "POST /v1/auth/login" {
						var n int
						var actor string
						if err := e.admin.QueryRow(ctx(t), `SELECT count(*), coalesce(min(actor_principal_id::text), '')
							FROM studio.audit_events WHERE request_id = $1 AND outcome = 'succeeded'`, res.requestID).
							Scan(&n, &actor); err != nil {
							t.Fatal(err)
						}
						if n != 1 || actor != c.id.String() {
							t.Errorf("%s: %d succeeded events (actor %s), want 1 by %s", route, n, actor, c.id)
						}
					}
				}
			})
		}
	}

	// 403s are audited: one aggregated denied event per (actor, action),
	// counting every refusal (plan 8.3).
	if err := e.agg.Flush(ctx(t)); err != nil {
		t.Fatal(err)
	}
	rows, err := e.admin.Query(ctx(t), `
		SELECT actor_principal_id::text, actor_kind::text, action, action_class, auth_method,
		       (details->>'repeat_count')::int, details->>'permission'
		FROM studio.audit_events WHERE outcome = 'denied'`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[denial]int{}
	for rows.Next() {
		var actor, kind, action, class, method, perm string
		var count int
		if err := rows.Scan(&actor, &kind, &action, &class, &method, &count, &perm); err != nil {
			t.Fatal(err)
		}
		got[denial{actor, action}] += count
		if perm != "manage_users" || (kind != "user" && kind != "service_account") ||
			(method != "session" && method != "api_key") {
			t.Errorf("denied event %s/%s: kind %s method %s permission %s", actor, action, kind, method, perm)
		}
		if actor == rootID.String() {
			t.Error("the platform administrator was denied")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(wantDenied) {
		t.Errorf("%d distinct denied (actor, action) pairs recorded, want %d", len(got), len(wantDenied))
	}
	for k, n := range wantDenied {
		if got[k] != n {
			t.Errorf("denied %s by %s: recorded %d, want %d", k.action, k.actor, got[k], n)
		}
	}
	// The matrix denies every non-admin principal every administration
	// route: 4 refused callers × 10 routes.
	if want := 4 * len(deniedAction); len(wantDenied) != want {
		t.Errorf("%d denied pairs exercised, want %d", len(wantDenied), want)
	}
	// Aggregation never updates audit rows; refused requests never write
	// a succeeded event.
	if n := countEvents(t, e.admin, `outcome = 'denied' AND actor_kind = 'anonymous'`); n != 0 {
		t.Errorf("%d anonymous denied events", n)
	}
}
