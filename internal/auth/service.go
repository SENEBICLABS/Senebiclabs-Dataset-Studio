package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/auth/authdb"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals"
)

// SessionLifetime is the absolute session lifetime (plan 14).
const SessionLifetime = 12 * time.Hour

// MaxUserAgentLength matches the sessions.user_agent CHECK.
const MaxUserAgentLength = 512

// Errors. Credential failures are deliberately indistinguishable (plan 14:
// "uniform error messages").
var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrUnauthenticated    = errors.New("auth: not authenticated")
	ErrNotSession         = errors.New("auth: not a session credential")
	ErrNotFound           = errors.New("auth: not found")
	ErrInvalidInput       = errors.New("auth: invalid input")
)

// RateLimitedError reports a login attempt refused by the rate limiter.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string { return "auth: too many login attempts" }

// Method is how a request authenticated.
type Method string

// Authentication methods (recorded as audit auth_method).
const (
	MethodSession  Method = "session"
	MethodAPIKey   Method = "api_key"
	MethodPassword Method = "password"
	MethodCLI      Method = "cli"
)

// Principal is the authenticated caller.
type Principal struct {
	ID        uuid.UUID
	Kind      audit.ActorKind
	Method    Method
	SessionID uuid.UUID // set for MethodSession
	APIKeyID  uuid.UUID // set for MethodAPIKey
}

// DB is what the service needs from a pool: queries plus transactions.
type DB interface {
	authdb.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// FailureRecorder receives failed-login events (the audit Aggregator).
type FailureRecorder interface {
	Add(ev audit.Event) error
}

// Config configures a Service.
type Config struct {
	// HMACKey keys identifier HMACs for anonymous failed logins. Required
	// for Login.
	HMACKey []byte
	// IdleTimeout ends sessions unused for this long.
	IdleTimeout time.Duration
	// Failures records failed logins out of band. Required for Login.
	Failures FailureRecorder
	// Now is the clock (tests inject one).
	Now func() time.Time
}

// Service implements authentication.
type Service struct {
	db       DB
	q        *authdb.Queries
	writer   *audit.Writer
	failures FailureRecorder
	hmacKey  []byte
	idle     time.Duration
	now      func() time.Time
	account  *keyedLimiter
	ip       *keyedLimiter
}

// NewService returns a Service using db (connected as studio_app).
func NewService(db DB, cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	return &Service{
		db: db, q: authdb.New(db), writer: audit.NewWriter(audit.Actions),
		failures: cfg.Failures, hmacKey: cfg.HMACKey, idle: cfg.IdleTimeout, now: cfg.Now,
		account: newKeyedLimiter(AccountAttemptsPerMinute, cfg.Now),
		ip:      newKeyedLimiter(IPAttemptsPerMinute, cfg.Now),
	}
}

func (s *Service) nowUTC() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

func (s *Service) inTx(ctx context.Context, fn func(tx pgx.Tx, q *authdb.Queries) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx, s.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth: commit: %w", err)
	}
	return nil
}

// LoginInput is one login attempt.
type LoginInput struct {
	Email, Password string
	IP              netip.Addr
	UserAgent       string
	RequestID       string
}

// LoginResult is a new session. Token is shown once and never stored.
type LoginResult struct {
	Token       string
	SessionID   uuid.UUID
	PrincipalID uuid.UUID
	ExpiresAt   time.Time
}

func normaliseEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// Login verifies a password and opens a session. Every refusal other than
// rate limiting returns ErrInvalidCredentials, whatever the cause, and every
// refusal is recorded as a failed auth.login_failed event.
func (s *Service) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	if s.failures == nil || len(s.hmacKey) == 0 {
		return LoginResult{}, errors.New("auth: login requires a failure recorder and an HMAC key")
	}
	email := normaliseEmail(in.Email)

	ipOK, ipWait := s.ip.allow("ip:" + in.IP.String())
	acctOK, acctWait := s.account.allow("acct:" + email)

	cand, lookupErr := s.q.GetLoginCandidate(ctx, email)
	known := lookupErr == nil
	if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
		return LoginResult{}, fmt.Errorf("auth: lookup: %w", lookupErr)
	}

	if !ipOK || !acctOK {
		s.recordFailure(in, email, known, cand.PrincipalID, "rate_limited")
		return LoginResult{}, &RateLimitedError{RetryAfter: max(ipWait, acctWait)}
	}

	hash := dummyHash
	if known && cand.PasswordHash != nil {
		hash = *cand.PasswordHash
	}
	ok, verr := false, error(nil)
	if len(in.Password) <= MaxPasswordLength {
		ok, verr = VerifyPassword(in.Password, hash)
	}
	switch {
	case verr != nil || !known || cand.PasswordHash == nil || !ok:
		s.recordFailure(in, email, known, cand.PrincipalID, "invalid_credentials")
		return LoginResult{}, ErrInvalidCredentials
	case cand.Status != "active":
		s.recordFailure(in, email, known, cand.PrincipalID, "disabled")
		return LoginResult{}, ErrInvalidCredentials
	}

	token, tokenHash, err := NewSessionToken()
	if err != nil {
		return LoginResult{}, err
	}
	sessionID, err := uuid.NewV7()
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: session id: %w", err)
	}
	now := s.nowUTC()
	res := LoginResult{Token: token, SessionID: sessionID, PrincipalID: cand.PrincipalID, ExpiresAt: now.Add(SessionLifetime)}
	var ipArg *netip.Addr
	if in.IP.IsValid() {
		ip := in.IP
		ipArg = &ip
	}
	var ua *string
	if in.UserAgent != "" {
		v := in.UserAgent
		if len(v) > MaxUserAgentLength {
			v = v[:MaxUserAgentLength]
		}
		ua = &v
	}
	err = s.inTx(ctx, func(tx pgx.Tx, q *authdb.Queries) error {
		if err := q.CreateSession(ctx, authdb.CreateSessionParams{
			ID: sessionID, PrincipalID: cand.PrincipalID, TokenHash: tokenHash,
			CreatedAt: now, ExpiresAt: res.ExpiresAt, Ip: ipArg, UserAgent: ua,
		}); err != nil {
			return fmt.Errorf("auth: create session: %w", err)
		}
		_, err := s.writer.Record(ctx, tx, audit.Event{
			Action: audit.ActionLogin, Outcome: audit.Succeeded,
			ActorPrincipalID: cand.PrincipalID.String(), ActorKind: audit.User, AuthMethod: string(MethodPassword),
			TargetType: "session", TargetID: sessionID.String(), RequestID: in.RequestID,
			Details: audit.LoginDetails{SessionExpiresAt: res.ExpiresAt.Format(time.RFC3339)},
		})
		return err
	})
	if err != nil {
		return LoginResult{}, err
	}
	return res, nil
}

// recordFailure hands a failed login to the aggregator. The actor is the
// existing user, or anonymous with the identifier HMAC (plan 8.1).
func (s *Service) recordFailure(in LoginInput, email string, known bool, principalID uuid.UUID, reason string) {
	ev := audit.Event{
		Action: audit.ActionLoginFailed, Outcome: audit.Failed, AuthMethod: string(MethodPassword),
		TargetType: "login", RequestID: in.RequestID,
	}
	if known {
		ev.ActorPrincipalID, ev.ActorKind = principalID.String(), audit.User
		ev.Details = audit.LoginFailedDetails{Reason: reason}
	} else {
		ev.ActorKind = audit.Anonymous
		ev.Details = audit.LoginFailedDetails{Reason: reason, IdentifierHMAC: IdentifierHMAC(s.hmacKey, email)}
	}
	_ = s.failures.Add(ev) // validated event; Add never blocks on the database
}

// Authenticate resolves a bearer credential (session token or API key).
// Every failure returns ErrUnauthenticated.
func (s *Service) Authenticate(ctx context.Context, bearer string) (Principal, error) {
	if bearer == "" {
		return Principal{}, ErrUnauthenticated
	}
	now := s.nowUTC()
	if IsAPIKey(bearer) {
		return s.authenticateAPIKey(ctx, bearer, now)
	}
	row, err := s.q.GetSessionByTokenHash(ctx, HashSecret(bearer))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Principal{}, ErrUnauthenticated
		}
		return Principal{}, fmt.Errorf("auth: session lookup: %w", err)
	}
	if row.RevokedAt != nil || !now.Before(row.ExpiresAt) || now.Sub(row.LastSeenAt) > s.idle ||
		row.Status != "active" || row.Kind != authdb.StudioActorKindUser {
		return Principal{}, ErrUnauthenticated
	}
	// At most once a minute; excluded from audit (plan 8.1).
	if err := s.q.TouchSession(ctx, authdb.TouchSessionParams{Now: now, ID: row.ID}); err != nil {
		return Principal{}, fmt.Errorf("auth: touch session: %w", err)
	}
	return Principal{ID: row.PrincipalID, Kind: audit.User, Method: MethodSession, SessionID: row.ID}, nil
}

func (s *Service) authenticateAPIKey(ctx context.Context, key string, now time.Time) (Principal, error) {
	prefix, err := ParseAPIKeyPrefix(key)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	row, err := s.q.GetAPIKeyByPrefix(ctx, prefix)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Principal{}, ErrUnauthenticated
		}
		return Principal{}, fmt.Errorf("auth: api key lookup: %w", err)
	}
	if subtle.ConstantTimeCompare(HashSecret(key), row.KeyHash) != 1 || row.RevokedAt != nil ||
		!now.Before(row.ExpiresAt) || row.Status != "active" {
		return Principal{}, ErrUnauthenticated
	}
	kind := audit.ActorKind(row.Kind)
	if kind != audit.User && kind != audit.ServiceAccount {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{ID: row.PrincipalID, Kind: kind, Method: MethodAPIKey, APIKeyID: row.ID}, nil
}

// Logout revokes the caller's session.
func (s *Service) Logout(ctx context.Context, p Principal, requestID string) error {
	if p.Method != MethodSession {
		return ErrNotSession
	}
	now := s.nowUTC()
	return s.inTx(ctx, func(tx pgx.Tx, q *authdb.Queries) error {
		n, err := q.RevokeSession(ctx, authdb.RevokeSessionParams{Now: &now, ID: p.SessionID})
		if err != nil {
			return fmt.Errorf("auth: revoke session: %w", err)
		}
		if n == 0 {
			return ErrUnauthenticated
		}
		_, err = s.writer.Record(ctx, tx, audit.Event{
			Action: audit.ActionLogout, Outcome: audit.Succeeded,
			ActorPrincipalID: p.ID.String(), ActorKind: p.Kind, AuthMethod: string(p.Method),
			TargetType: "session", TargetID: p.SessionID.String(), RequestID: requestID,
			Details: audit.LogoutDetails{},
		})
		return err
	})
}

// IssuedAPIKey is a new key. Key is shown once and never stored.
type IssuedAPIKey struct {
	ID        uuid.UUID
	Key       string
	Prefix    string
	ExpiresAt time.Time
}

// IssueAPIKey creates an API key for principalID on behalf of actor. Whether
// actor may do so is an authorization decision (M4); this method only
// authenticates and records it.
func (s *Service) IssueAPIKey(ctx context.Context, actor Principal, principalID uuid.UUID, expiresAt time.Time, requestID string) (IssuedAPIKey, error) {
	now := s.nowUTC()
	expiresAt = expiresAt.UTC().Truncate(time.Microsecond)
	if !expiresAt.After(now) {
		return IssuedAPIKey{}, fmt.Errorf("%w: expiry must be in the future", ErrInvalidInput)
	}
	target, err := s.q.GetPrincipalForKey(ctx, principalID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IssuedAPIKey{}, ErrNotFound
		}
		return IssuedAPIKey{}, fmt.Errorf("auth: principal lookup: %w", err)
	}
	if target.Kind != authdb.StudioActorKindUser && target.Kind != authdb.StudioActorKindServiceAccount {
		return IssuedAPIKey{}, fmt.Errorf("%w: system principals cannot hold API keys", ErrInvalidInput)
	}
	key, prefix, hash, err := NewAPIKey()
	if err != nil {
		return IssuedAPIKey{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return IssuedAPIKey{}, fmt.Errorf("auth: key id: %w", err)
	}
	err = s.inTx(ctx, func(tx pgx.Tx, q *authdb.Queries) error {
		if err := q.CreateAPIKey(ctx, authdb.CreateAPIKeyParams{
			ID: id, PrincipalID: principalID, PrincipalKind: target.Kind, Prefix: prefix, KeyHash: hash,
			CreatedAt: now, CreatedBy: actor.ID, ExpiresAt: expiresAt,
		}); err != nil {
			return fmt.Errorf("auth: create api key: %w", err)
		}
		_, err := s.writer.Record(ctx, tx, audit.Event{
			Action: audit.ActionAPIKeyCreated, Outcome: audit.Succeeded,
			ActorPrincipalID: actor.ID.String(), ActorKind: actor.Kind, AuthMethod: string(actor.Method),
			TargetType: "api_key", TargetID: id.String(), RequestID: requestID,
			Details: audit.APIKeyDetails{Prefix: prefix, PrincipalID: principalID.String(), ExpiresAt: expiresAt.Format(time.RFC3339)},
		})
		return err
	})
	if err != nil {
		return IssuedAPIKey{}, err
	}
	return IssuedAPIKey{ID: id, Key: key, Prefix: prefix, ExpiresAt: expiresAt}, nil
}

// RevokeAPIKey permanently revokes a key on behalf of actor.
func (s *Service) RevokeAPIKey(ctx context.Context, actor Principal, keyID uuid.UUID, requestID string) error {
	now := s.nowUTC()
	return s.inTx(ctx, func(tx pgx.Tx, q *authdb.Queries) error {
		row, err := q.RevokeAPIKey(ctx, authdb.RevokeAPIKeyParams{Now: &now, ID: keyID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("auth: revoke api key: %w", err)
		}
		_, err = s.writer.Record(ctx, tx, audit.Event{
			Action: audit.ActionAPIKeyRevoked, Outcome: audit.Succeeded,
			ActorPrincipalID: actor.ID.String(), ActorKind: actor.Kind, AuthMethod: string(actor.Method),
			TargetType: "api_key", TargetID: keyID.String(), RequestID: requestID,
			Details: audit.APIKeyDetails{Prefix: row.Prefix, PrincipalID: row.PrincipalID.String()},
		})
		return err
	})
}

// AdminInput creates a platform administrator through studioctl.
type AdminInput struct {
	Email, DisplayName, Password string
	OSUser, Hostname             string
}

// CreateAdmin creates a user with global role platform_admin as system:cli
// (plan 14: the first admin is created only through studioctl; plan 7.2:
// created_by and the audit actor are system:cli).
func (s *Service) CreateAdmin(ctx context.Context, in AdminInput) (uuid.UUID, error) {
	email := normaliseEmail(in.Email)
	if len(email) < 3 || len(email) > 254 || !strings.Contains(email, "@") || strings.ContainsAny(email, " \t\r\n") {
		return uuid.Nil, fmt.Errorf("%w: email must be a single address of at most 254 characters", ErrInvalidInput)
	}
	name := strings.TrimSpace(in.DisplayName)
	if name == "" || len(name) > 200 {
		return uuid.Nil, fmt.Errorf("%w: display name must be 1-200 characters", ErrInvalidInput)
	}
	if err := ValidatePassword(in.Password); err != nil {
		return uuid.Nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	hash, err := HashPassword(in.Password)
	if err != nil {
		return uuid.Nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: principal id: %w", err)
	}
	cli := uuid.MustParse(principals.CLIID)
	const role = "platform_admin"
	err = s.inTx(ctx, func(tx pgx.Tx, q *authdb.Queries) error {
		if err := q.CreatePrincipal(ctx, authdb.CreatePrincipalParams{
			ID: id, Kind: authdb.StudioActorKindUser, Handle: "user:" + email, CreatedBy: cli,
		}); err != nil {
			return fmt.Errorf("auth: create principal: %w", err)
		}
		if err := q.CreateUser(ctx, authdb.CreateUserParams{
			PrincipalID: id, Email: email, DisplayName: name, PasswordHash: &hash, GlobalRole: role, CreatedBy: cli,
		}); err != nil {
			return fmt.Errorf("auth: create user: %w", err)
		}
		_, err := s.writer.Record(ctx, tx, audit.Event{
			Action: audit.ActionUserCreated, Outcome: audit.Succeeded,
			ActorPrincipalID: principals.CLIID, ActorKind: audit.System, AuthMethod: string(MethodCLI),
			TargetType: "user", TargetID: id.String(),
			Details: audit.UserCreatedDetails{GlobalRole: role, OSUser: in.OSUser, Hostname: in.Hostname},
		})
		return err
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}
