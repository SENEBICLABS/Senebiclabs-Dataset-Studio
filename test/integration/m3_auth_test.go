//go:build integration

package integration

import (
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/auth"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/testdb"
)

var testHMACKey = []byte("integration-only-hmac-key-0123456789")

const adminPassword = "admin-password-for-tests"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type authEnv struct {
	db    *testdb.DB
	admin *pgx.Conn // superuser, for inspection only
	svc   *auth.Service
	agg   *audit.Aggregator
	clk   *clock
}

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	db := testdb.New(t)
	pool := appPool(t, db)
	clk := &clock{t: time.Now().UTC()}
	agg := audit.NewAggregator(pool, slog.New(slog.DiscardHandler), audit.Actions, audit.AggregatorConfig{Window: time.Hour})
	svc := auth.NewService(pool, auth.Config{HMACKey: testHMACKey, IdleTimeout: 30 * time.Minute, Failures: agg, Now: clk.now})
	return &authEnv{db: db, admin: db.Connect(t, testdb.Admin), svc: svc, agg: agg, clk: clk}
}

func (e *authEnv) createAdmin(t *testing.T, email string) uuid.UUID {
	t.Helper()
	id, err := e.svc.CreateAdmin(ctx(t), auth.AdminInput{Email: email, DisplayName: "Admin", Password: adminPassword,
		OSUser: "tester", Hostname: "test-host"})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	return id
}

func (e *authEnv) login(t *testing.T, email, password string) (auth.LoginResult, error) {
	t.Helper()
	return e.svc.Login(ctx(t), auth.LoginInput{Email: email, Password: password,
		IP: netip.MustParseAddr("192.0.2.10"), UserAgent: "test-agent", RequestID: "req-1"})
}

// rowsContaining reports any audit_events, sessions or api_keys row whose text
// form contains s (used to prove secrets are never stored).
func (e *authEnv) rowsContaining(t *testing.T, s string) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(ctx(t), `
		SELECT (SELECT count(*) FROM studio.audit_events a WHERE a::text LIKE '%' || $1 || '%')
		     + (SELECT count(*) FROM studio.sessions x WHERE x::text LIKE '%' || $1 || '%')
		     + (SELECT count(*) FROM studio.api_keys k WHERE k::text LIKE '%' || $1 || '%')
		     + (SELECT count(*) FROM studio.users u WHERE u::text LIKE '%' || $1 || '%')`, s).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// studioctl create-admin path: a platform_admin user created by system:cli,
// with an argon2id hash and a user.created event naming the operator.
func TestCreateAdmin(t *testing.T) {
	e := newAuthEnv(t)
	id := e.createAdmin(t, " Admin@Example.TEST ")

	var kind, handle, createdBy, role, hash, email string
	if err := e.admin.QueryRow(ctx(t), `
		SELECT p.kind::text, p.handle, p.created_by::text, u.global_role, u.password_hash, u.email::text
		FROM studio.principals p JOIN studio.users u ON u.principal_id = p.id WHERE p.id = $1`, id).
		Scan(&kind, &handle, &createdBy, &role, &hash, &email); err != nil {
		t.Fatal(err)
	}
	if kind != "user" || createdBy != principals.CLIID || role != "platform_admin" ||
		email != "admin@example.test" || handle != "user:admin@example.test" {
		t.Errorf("admin row: %s %s %s %s %s", kind, handle, createdBy, role, email)
	}
	if !strings.HasPrefix(hash, "$argon2id$") || e.rowsContaining(t, adminPassword) != 0 {
		t.Error("password stored in a recoverable form")
	}
	var actor, method, osUser string
	if err := e.admin.QueryRow(ctx(t), `SELECT actor_principal_id::text, auth_method, details->>'os_user'
		FROM studio.audit_events WHERE action = 'user.created' AND target_id = $1`, id.String()).
		Scan(&actor, &method, &osUser); err != nil {
		t.Fatal(err)
	}
	if actor != principals.CLIID || method != "cli" || osUser != "tester" {
		t.Errorf("user.created: actor %s method %s os_user %s", actor, method, osUser)
	}

	if _, err := e.svc.CreateAdmin(ctx(t), auth.AdminInput{Email: "admin@example.test", DisplayName: "Dup",
		Password: adminPassword}); err == nil {
		t.Error("duplicate admin email accepted")
	}
	if _, err := e.svc.CreateAdmin(ctx(t), auth.AdminInput{Email: "x@example.test", DisplayName: "X", Password: "short"}); !errors.Is(err, auth.ErrInvalidInput) {
		t.Errorf("short password: %v", err)
	}
}

// Session lifecycle: login -> authenticate -> logout -> rejected; only the
// SHA-256 of the token is stored; login and logout are audited.
func TestSessionLifecycleStoresOnlyHashes(t *testing.T) {
	e := newAuthEnv(t)
	adminID := e.createAdmin(t, "admin@example.test")

	res, err := e.login(t, "ADMIN@example.test", adminPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.PrincipalID != adminID || !res.ExpiresAt.Equal(e.clk.now().Truncate(time.Microsecond).Add(12*time.Hour)) {
		t.Errorf("login result %+v", res)
	}
	if e.rowsContaining(t, res.Token) != 0 {
		t.Fatal("the session token is stored somewhere in plaintext")
	}
	var stored []byte
	if err := e.admin.QueryRow(ctx(t), `SELECT token_hash FROM studio.sessions WHERE id = $1`, res.SessionID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(auth.HashSecret(res.Token)) {
		t.Error("stored hash is not SHA-256 of the token")
	}

	p, err := e.svc.Authenticate(ctx(t), res.Token)
	if err != nil || p.ID != adminID || p.Method != auth.MethodSession || p.SessionID != res.SessionID || p.Kind != audit.User {
		t.Fatalf("authenticate: %+v %v", p, err)
	}
	if err := e.svc.Logout(ctx(t), p, "req-2"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := e.svc.Authenticate(ctx(t), res.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("revoked session still authenticates: %v", err)
	}
	if err := e.svc.Logout(ctx(t), p, "req-3"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("second logout: %v", err)
	}
	if n := countEvents(t, e.admin, `action = 'auth.login' AND actor_principal_id = $1 AND target_id = $2 AND outcome = 'succeeded'`,
		adminID, res.SessionID.String()); n != 1 {
		t.Errorf("%d auth.login events", n)
	}
	if n := countEvents(t, e.admin, `action = 'auth.logout' AND actor_principal_id = $1 AND auth_method = 'session'`, adminID); n != 1 {
		t.Errorf("%d auth.logout events", n)
	}
	if _, err := e.svc.Authenticate(ctx(t), "not-a-token"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("garbage token: %v", err)
	}
}

// Sessions end after the idle timeout and, even when used, after 12 hours.
func TestSessionIdleTimeoutAndAbsoluteExpiry(t *testing.T) {
	e := newAuthEnv(t)
	e.createAdmin(t, "admin@example.test")

	idle, err := e.login(t, "admin@example.test", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.advance(31 * time.Minute)
	if _, err := e.svc.Authenticate(ctx(t), idle.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("idle session accepted: %v", err)
	}

	active, err := e.login(t, "admin@example.test", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	for elapsed := time.Duration(0); elapsed < 12*time.Hour-20*time.Minute; elapsed += 20 * time.Minute {
		e.clk.advance(20 * time.Minute)
		if _, err := e.svc.Authenticate(ctx(t), active.Token); err != nil {
			t.Fatalf("active session rejected after %s: %v", elapsed+20*time.Minute, err)
		}
	}
	e.clk.advance(20 * time.Minute) // now past 12 hours since login
	if _, err := e.svc.Authenticate(ctx(t), active.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("session accepted beyond its 12-hour lifetime: %v", err)
	}
}

// Failed logins: one uniform error for every cause; audited out of band with
// the existing user as actor, or anonymous with the identifier HMAC only.
func TestFailedLoginsAreUniformAndAudited(t *testing.T) {
	e := newAuthEnv(t)
	adminID := e.createAdmin(t, "admin@example.test")
	disabledID := e.createAdmin(t, "disabled@example.test")
	if _, err := e.admin.Exec(ctx(t), `UPDATE studio.principals SET status = 'disabled' WHERE id = $1`, disabledID); err != nil {
		t.Fatal(err)
	}

	for name, try := range map[string][2]string{
		"wrong password": {"admin@example.test", "wrong-password-123"},
		"unknown user":   {"nobody@example.test", adminPassword},
		"disabled user":  {"disabled@example.test", adminPassword},
		"over-long":      {"admin@example.test", strings.Repeat("x", auth.MaxPasswordLength+1)},
	} {
		if _, err := e.login(t, try[0], try[1]); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Errorf("%s: got %v, want the uniform ErrInvalidCredentials", name, err)
		}
	}
	if err := e.agg.Flush(ctx(t)); err != nil {
		t.Fatal(err)
	}

	type ev struct{ actor, kind, reason, hmac string }
	rows, err := e.admin.Query(ctx(t), `
		SELECT coalesce(actor_principal_id::text, ''), actor_kind::text, details->>'reason',
		       coalesce(details->>'identifier_hmac', '')
		FROM studio.audit_events WHERE action = 'auth.login_failed' AND outcome = 'failed'
		ORDER BY actor_kind::text, details->>'reason'`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ev, error) {
		var x ev
		return x, r.Scan(&x.actor, &x.kind, &x.reason, &x.hmac)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Ordered by actor_kind as text, then reason.
	want := []ev{
		{"", "anonymous", "invalid_credentials", auth.IdentifierHMAC(testHMACKey, "nobody@example.test")},
		{disabledID.String(), "user", "disabled", ""},
		{adminID.String(), "user", "invalid_credentials", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("failed-login events:\n got %+v\nwant %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	if e.rowsContaining(t, "nobody@example.test") != 0 {
		t.Error("the raw unknown identifier was recorded")
	}
	var repeats int
	if err := e.admin.QueryRow(ctx(t), `SELECT (details->>'repeat_count')::int FROM studio.audit_events
		WHERE action = 'auth.login_failed' AND actor_principal_id = $1`, adminID).Scan(&repeats); err != nil || repeats != 2 {
		t.Errorf("wrong-password and over-long attempts aggregate into one event: repeat_count %d %v", repeats, err)
	}
}

// Login is rate-limited per account and per IP.
func TestLoginRateLimitedPerAccountAndIP(t *testing.T) {
	e := newAuthEnv(t)
	e.createAdmin(t, "admin@example.test")
	var limited *auth.RateLimitedError
	for i := range auth.AccountAttemptsPerMinute {
		if _, err := e.login(t, "admin@example.test", "wrong-password-123"); errors.As(err, &limited) {
			t.Fatalf("attempt %d rate-limited too early", i+1)
		}
	}
	if _, err := e.login(t, "admin@example.test", adminPassword); !errors.As(err, &limited) || limited.RetryAfter <= 0 {
		t.Fatalf("account limit not enforced (even with the right password): %v", err)
	}
	// Other accounts from the same IP continue until the IP limit.
	for i := auth.AccountAttemptsPerMinute + 1; i < auth.IPAttemptsPerMinute; i++ {
		if _, err := e.login(t, "other"+string(rune('a'+i))+"@example.test", "x-password-123"); errors.As(err, &limited) {
			t.Fatalf("IP limited after %d attempts, limit is %d", i, auth.IPAttemptsPerMinute)
		}
	}
	if _, err := e.login(t, "fresh@example.test", "x-password-123"); !errors.As(err, &limited) {
		t.Fatalf("IP limit not enforced: %v", err)
	}
	e.clk.advance(time.Minute)
	if _, err := e.login(t, "admin@example.test", adminPassword); err != nil {
		t.Errorf("limit did not recover: %v", err)
	}
}

// API key lifecycle: issue (hash only) -> authenticate -> revoke; expiry,
// disabled principals and system principals are refused.
func TestAPIKeyLifecycle(t *testing.T) {
	e := newAuthEnv(t)
	adminID := e.createAdmin(t, "admin@example.test")
	actor := auth.Principal{ID: adminID, Kind: audit.User, Method: auth.MethodSession}
	svcID := uuid.MustParse("0190a6e4-0000-7000-8000-0000000005a1")
	if _, err := e.admin.Exec(ctx(t), `
		WITH p AS (INSERT INTO studio.principals (id, kind, handle, created_by) VALUES ($1, 'service_account', 'svc:loader', $2) RETURNING id)
		INSERT INTO studio.service_accounts (principal_id, created_by) SELECT id, $2 FROM p`, svcID, adminID); err != nil {
		t.Fatal(err)
	}

	issued, err := e.svc.IssueAPIKey(ctx(t), actor, svcID, e.clk.now().Add(time.Hour), "req-k1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if e.rowsContaining(t, strings.TrimPrefix(issued.Key, "dsk_"+issued.Prefix+"_")) != 0 {
		t.Fatal("the API key secret is stored somewhere in plaintext")
	}
	var createdBy string
	if err := e.admin.QueryRow(ctx(t), `SELECT created_by::text FROM studio.api_keys WHERE id = $1`, issued.ID).Scan(&createdBy); err != nil || createdBy != adminID.String() {
		t.Errorf("api_keys.created_by = %s (%v), want the issuing admin", createdBy, err)
	}

	p, err := e.svc.Authenticate(ctx(t), issued.Key)
	if err != nil || p.ID != svcID || p.Kind != audit.ServiceAccount || p.Method != auth.MethodAPIKey || p.APIKeyID != issued.ID {
		t.Fatalf("authenticate key: %+v %v", p, err)
	}
	forged := "dsk_" + issued.Prefix + "_" + strings.Repeat("A", 43)
	if _, err := e.svc.Authenticate(ctx(t), forged); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("forged secret with a real prefix accepted: %v", err)
	}
	if err := e.svc.Logout(ctx(t), p, "req-x"); !errors.Is(err, auth.ErrNotSession) {
		t.Errorf("logout with an API key: %v", err)
	}

	if err := e.svc.RevokeAPIKey(ctx(t), actor, issued.ID, "req-k2"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx(t), issued.Key); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("revoked key accepted: %v", err)
	}
	if err := e.svc.RevokeAPIKey(ctx(t), actor, issued.ID, "req-k3"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("second revoke: %v", err)
	}
	for _, action := range []string{"auth.api_key_created", "auth.api_key_revoked"} {
		if n := countEvents(t, e.admin, `action = $1 AND actor_principal_id = $2 AND target_id = $3 AND details->>'prefix' = $4`,
			action, adminID, issued.ID.String(), issued.Prefix); n != 1 {
			t.Errorf("%d %s events", n, action)
		}
	}

	short, err := e.svc.IssueAPIKey(ctx(t), actor, svcID, e.clk.now().Add(time.Minute), "req-k4")
	if err != nil {
		t.Fatal(err)
	}
	e.clk.advance(2 * time.Minute)
	if _, err := e.svc.Authenticate(ctx(t), short.Key); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("expired key accepted: %v", err)
	}

	live, err := e.svc.IssueAPIKey(ctx(t), actor, svcID, e.clk.now().Add(time.Hour), "req-k5")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.Exec(ctx(t), `UPDATE studio.principals SET status = 'disabled' WHERE id = $1`, svcID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx(t), live.Key); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("key of a disabled principal accepted: %v", err)
	}

	if _, err := e.svc.IssueAPIKey(ctx(t), actor, uuid.MustParse(principals.WorkerID), e.clk.now().Add(time.Hour), "req-k6"); !errors.Is(err, auth.ErrInvalidInput) {
		t.Errorf("API key for a system principal: %v", err)
	}
	if _, err := e.svc.IssueAPIKey(ctx(t), actor, svcID, e.clk.now().Add(-time.Minute), "req-k7"); !errors.Is(err, auth.ErrInvalidInput) {
		t.Errorf("API key expiring in the past: %v", err)
	}
}

// The database enforces credential rules regardless of application code.
func TestCredentialTablesEnforceRulesInTheDatabase(t *testing.T) {
	e := newAuthEnv(t)
	adminID := e.createAdmin(t, "admin@example.test")
	res, err := e.login(t, "admin@example.test", adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	hash := make([]byte, 32)

	wantSQLState(t, exec(t, e.admin, `INSERT INTO studio.sessions (principal_id, principal_kind, token_hash, expires_at)
		VALUES ($1, 'system', $2, now() + interval '1 hour')`, principals.WorkerID, hash),
		checkViolation, "sessions_users_only", "session for a system principal")
	wantSQLState(t, exec(t, e.admin, `INSERT INTO studio.sessions (principal_id, token_hash, expires_at)
		VALUES ($1, $2, now() + interval '1 hour')`, principals.WorkerID, hash),
		foreignKeyViolation, "sessions_principal_fkey", "session claiming user kind for a system principal")
	wantSQLState(t, exec(t, e.admin, `INSERT INTO studio.api_keys (principal_id, principal_kind, prefix, key_hash, created_by, expires_at)
		VALUES ($1, 'system', 'abcdefgh', $2, $1, now() + interval '1 hour')`, principals.WorkerID, hash),
		checkViolation, "api_keys_no_system_principals", "API key for a system principal")
	wantSQLState(t, exec(t, e.admin, `INSERT INTO studio.sessions (principal_id, token_hash, expires_at)
		VALUES ($1, '\x00'::bytea, now() + interval '1 hour')`, adminID),
		checkViolation, "sessions_token_hash_sha256", "non-SHA-256 token hash")

	wantSQLState(t, exec(t, e.admin, `UPDATE studio.sessions SET token_hash = $2 WHERE id = $1`, res.SessionID, hash),
		restrictViolation, "", "rewriting a token hash")
	wantSQLState(t, exec(t, e.admin, `UPDATE studio.sessions SET expires_at = expires_at + interval '1 day' WHERE id = $1`, res.SessionID),
		restrictViolation, "", "extending a session")
	if _, err := e.admin.Exec(ctx(t), `UPDATE studio.sessions SET revoked_at = now() WHERE id = $1`, res.SessionID); err != nil {
		t.Fatal(err)
	}
	wantSQLState(t, exec(t, e.admin, `UPDATE studio.sessions SET revoked_at = NULL WHERE id = $1`, res.SessionID),
		restrictViolation, "", "un-revoking a session")

	app := e.db.Connect(t, dbroles.App)
	wantSQLState(t, exec(t, app, `UPDATE studio.sessions SET token_hash = $2 WHERE id = $1`, res.SessionID, hash),
		insufficientPriv, "", "app rewriting a token hash")
	wantSQLState(t, exec(t, app, `DELETE FROM studio.sessions`), insufficientPriv, "", "app deleting sessions")
	for _, role := range []string{dbroles.Worker, dbroles.ReadOnly} {
		conn := e.db.Connect(t, role)
		for _, table := range []string{"sessions", "api_keys"} {
			wantSQLState(t, exec(t, conn, `SELECT 1 FROM studio.`+table), insufficientPriv, "", role+" reading "+table)
		}
	}
}
