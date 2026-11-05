// Package admin administers users, service accounts and API keys (plan 15:
// Users; Service accounts and keys). Every operation needs the "Manage users
// and service accounts" permission (plan 14), checked here in the service
// layer before any read, so a refused caller learns nothing about the
// resource and every refusal is a denied audit event.
//
// Each mutation is one transaction with exactly one succeeded audit event
// (plan 8.1). created_by and the audit actor are the authenticated caller
// (plan 7.2).
package admin

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/auth"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/authz"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals/admin/admindb"
)

// Errors. Input errors wrap auth.ErrInvalidInput.
var (
	ErrNotFound = errors.New("admin: not found")
	ErrConflict = errors.New("admin: conflict")
)

// Page limits for list endpoints (cursor pagination, plan 15).
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// DB is what the service needs from a pool.
type DB interface {
	admindb.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Service implements the administration operations.
type Service struct {
	db     DB
	q      *admindb.Queries
	writer *audit.Writer
	authz  *authz.Authorizer
	auth   *auth.Service
	now    func() time.Time
}

// NewService returns a Service. db connects as studio_app; authSvc issues and
// revokes API keys once this service has authorized the call.
func NewService(db DB, az *authz.Authorizer, authSvc *auth.Service, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, q: admindb.New(db), writer: audit.NewWriter(audit.Actions), authz: az, auth: authSvc, now: now}
}

func (s *Service) nowUTC() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// authorize checks ManageUsers for every administration operation.
func (s *Service) authorize(actor auth.Principal, action, targetType, targetID, requestID string) error {
	return s.authz.Authorize(actor.Subject(), authz.ManageUsers, authz.Global(), authz.Denial{
		Action: action, TargetType: targetType, TargetID: targetID, RequestID: requestID,
	})
}

func (s *Service) inTx(ctx context.Context, fn func(tx pgx.Tx, q *admindb.Queries) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("admin: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx, s.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("admin: commit: %w", err)
	}
	return nil
}

func (s *Service) record(ctx context.Context, tx pgx.Tx, actor auth.Principal, action, targetType, targetID, requestID string, details any) error {
	_, err := s.writer.Record(ctx, tx, audit.Event{
		Action: action, Outcome: audit.Succeeded,
		ActorPrincipalID: actor.ID.String(), ActorKind: actor.Kind, AuthMethod: string(actor.Method),
		TargetType: targetType, TargetID: targetID, RequestID: requestID, Details: details,
	})
	return err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", auth.ErrInvalidInput, fmt.Sprintf(format, args...))
}

func validRole(r string) (authz.GlobalRole, error) {
	role := authz.GlobalRole(r)
	if !authz.ValidGlobalRole(role) {
		return "", invalid("global_role must be one of platform_admin, registry_admin, member")
	}
	return role, nil
}

// Page selects one page of a list.
type Page struct {
	Limit  int
	Cursor string
}

func (p Page) decode() (uuid.UUID, int32, error) {
	limit := p.Limit
	switch {
	case limit == 0:
		limit = DefaultPageSize
	case limit < 1 || limit > MaxPageSize:
		return uuid.Nil, 0, invalid("limit must be between 1 and %d", MaxPageSize)
	}
	// limit is in [1, MaxPageSize] here, so the int32 conversion is exact.
	if p.Cursor == "" {
		return uuid.Nil, int32(limit), nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(p.Cursor)
	if err != nil || len(raw) != 16 {
		return uuid.Nil, 0, invalid("cursor is not valid")
	}
	return uuid.UUID(raw), int32(limit), nil
}

func nextCursor(n, limit int, last uuid.UUID) string {
	if n < limit {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(last[:])
}

// User is a user as the API shows it. It never carries the password hash.
type User struct {
	ID          uuid.UUID
	Email       string
	DisplayName string
	GlobalRole  string
	Status      string
	CreatedAt   time.Time
}

func userFrom(r admindb.GetUserRow) User {
	return User{ID: r.PrincipalID, Email: r.Email, DisplayName: r.DisplayName, GlobalRole: r.GlobalRole,
		Status: r.Status, CreatedAt: r.CreatedAt}
}

// ListUsers returns one page of users in creation order.
func (s *Service) ListUsers(ctx context.Context, actor auth.Principal, page Page, requestID string) ([]User, string, error) {
	if err := s.authorize(actor, audit.ActionUserList, "user", "", requestID); err != nil {
		return nil, "", err
	}
	after, limit, err := page.decode()
	if err != nil {
		return nil, "", err
	}
	rows, err := s.q.ListUsers(ctx, admindb.ListUsersParams{After: after, RowLimit: limit})
	if err != nil {
		return nil, "", fmt.Errorf("admin: list users: %w", err)
	}
	out := make([]User, len(rows))
	for i, r := range rows {
		out[i] = userFrom(admindb.GetUserRow(r))
	}
	next := ""
	if len(out) > 0 {
		next = nextCursor(len(out), int(limit), out[len(out)-1].ID)
	}
	return out, next, nil
}

// GetUser returns one user.
func (s *Service) GetUser(ctx context.Context, actor auth.Principal, id uuid.UUID, requestID string) (User, error) {
	if err := s.authorize(actor, audit.ActionUserRead, "user", id.String(), requestID); err != nil {
		return User{}, err
	}
	row, err := s.q.GetUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("admin: get user: %w", err)
	}
	return userFrom(row), nil
}

// CreateUserInput creates a user with a password.
type CreateUserInput struct {
	Email, DisplayName, Password, GlobalRole string
}

// CreateUser creates a user. The caller is created_by and the audit actor.
func (s *Service) CreateUser(ctx context.Context, actor auth.Principal, in CreateUserInput, requestID string) (User, error) {
	if err := s.authorize(actor, audit.ActionUserCreated, "user", "", requestID); err != nil {
		return User{}, err
	}
	email, err := auth.ValidateEmail(in.Email)
	if err != nil {
		return User{}, err
	}
	name, err := auth.ValidateDisplayName(in.DisplayName)
	if err != nil {
		return User{}, err
	}
	if in.GlobalRole == "" {
		in.GlobalRole = string(authz.Member)
	}
	role, err := validRole(in.GlobalRole)
	if err != nil {
		return User{}, err
	}
	if err := auth.ValidatePassword(in.Password); err != nil {
		return User{}, fmt.Errorf("%w: %w", auth.ErrInvalidInput, err)
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return User{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return User{}, fmt.Errorf("admin: principal id: %w", err)
	}
	now := s.nowUTC()
	err = s.inTx(ctx, func(tx pgx.Tx, q *admindb.Queries) error {
		if err := q.CreatePrincipal(ctx, admindb.CreatePrincipalParams{
			ID: id, Kind: admindb.StudioActorKindUser, Handle: auth.UserHandle(email), CreatedBy: actor.ID,
		}); err != nil {
			return fmt.Errorf("admin: create principal: %w", err)
		}
		if err := q.CreateUser(ctx, admindb.CreateUserParams{
			PrincipalID: id, Email: email, DisplayName: name, PasswordHash: &hash, GlobalRole: string(role),
			CreatedAt: now, CreatedBy: actor.ID,
		}); err != nil {
			return fmt.Errorf("admin: create user: %w", err)
		}
		return s.record(ctx, tx, actor, audit.ActionUserCreated, "user", id.String(), requestID,
			audit.UserCreatedDetails{GlobalRole: string(role)})
	})
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, fmt.Errorf("%w: a user with this email already exists", ErrConflict)
		}
		return User{}, err
	}
	return User{ID: id, Email: email, DisplayName: name, GlobalRole: string(role), Status: "active", CreatedAt: now}, nil
}

// UpdateUserInput changes a user. Nil fields are left unchanged. Email is
// not changeable (plan 6.3 grants U users(display_name, global_role,
// password_hash) only).
type UpdateUserInput struct {
	DisplayName *string
	GlobalRole  *string
	Password    *string
}

// UpdateUser applies in. A password change also revokes the user's open
// sessions in the same transaction, so a reset password ends existing logins.
func (s *Service) UpdateUser(ctx context.Context, actor auth.Principal, id uuid.UUID, in UpdateUserInput, requestID string) (User, error) {
	if err := s.authorize(actor, audit.ActionUserUpdated, "user", id.String(), requestID); err != nil {
		return User{}, err
	}
	params := admindb.UpdateUserParams{ID: id}
	var fields []string
	details := audit.UserUpdatedDetails{}
	if in.DisplayName != nil {
		name, err := auth.ValidateDisplayName(*in.DisplayName)
		if err != nil {
			return User{}, err
		}
		params.DisplayName = &name
		fields = append(fields, "display_name")
	}
	if in.GlobalRole != nil {
		role, err := validRole(*in.GlobalRole)
		if err != nil {
			return User{}, err
		}
		r := string(role)
		params.GlobalRole = &r
		details.GlobalRole = r
		fields = append(fields, "global_role")
	}
	if in.Password != nil {
		if err := auth.ValidatePassword(*in.Password); err != nil {
			return User{}, fmt.Errorf("%w: %w", auth.ErrInvalidInput, err)
		}
		hash, err := auth.HashPassword(*in.Password)
		if err != nil {
			return User{}, err
		}
		params.PasswordHash = &hash
		fields = append(fields, "password")
	}
	if len(fields) == 0 {
		return User{}, invalid("nothing to change: set display_name, global_role or password")
	}
	slices.Sort(fields)
	details.Fields = fields
	now := s.nowUTC()
	var out User
	err := s.inTx(ctx, func(tx pgx.Tx, q *admindb.Queries) error {
		n, err := q.UpdateUser(ctx, params)
		if err != nil {
			return fmt.Errorf("admin: update user: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
		if params.PasswordHash != nil {
			if details.SessionsRevoked, err = q.RevokePrincipalSessions(ctx, admindb.RevokePrincipalSessionsParams{
				Now: &now, PrincipalID: id,
			}); err != nil {
				return fmt.Errorf("admin: revoke sessions: %w", err)
			}
		}
		row, err := q.GetUser(ctx, id)
		if err != nil {
			return fmt.Errorf("admin: reread user: %w", err)
		}
		out = userFrom(row)
		return s.record(ctx, tx, actor, audit.ActionUserUpdated, "user", id.String(), requestID, details)
	})
	return out, err
}

// DisableUser disables a user and revokes its open sessions. Its API keys
// stop working at once because authentication checks the principal's status.
func (s *Service) DisableUser(ctx context.Context, actor auth.Principal, id uuid.UUID, requestID string) (User, error) {
	if err := s.authorize(actor, audit.ActionUserDisabled, "user", id.String(), requestID); err != nil {
		return User{}, err
	}
	now := s.nowUTC()
	var out User
	err := s.inTx(ctx, func(tx pgx.Tx, q *admindb.Queries) error {
		n, err := q.DisablePrincipal(ctx, admindb.DisablePrincipalParams{ID: id, Kind: admindb.StudioActorKindUser})
		if err != nil {
			return fmt.Errorf("admin: disable user: %w", err)
		}
		row, err := q.GetUser(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("admin: reread user: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: the user is already disabled", ErrConflict)
		}
		revoked, err := q.RevokePrincipalSessions(ctx, admindb.RevokePrincipalSessionsParams{Now: &now, PrincipalID: id})
		if err != nil {
			return fmt.Errorf("admin: revoke sessions: %w", err)
		}
		out = userFrom(row)
		return s.record(ctx, tx, actor, audit.ActionUserDisabled, "user", id.String(), requestID,
			audit.UserDisabledDetails{SessionsRevoked: revoked})
	})
	return out, err
}

// ServiceAccount is a service account as the API shows it. Name is known
// only when the account is created: it is stored as the principal handle,
// which studio_app may not read back (plan 6.3, test G2).
type ServiceAccount struct {
	ID          uuid.UUID
	Name        string
	Description string
	GlobalRole  string
	Status      string
	CreatedAt   time.Time
}

// ServiceAccountHandlePrefix prefixes service-account principal handles.
const ServiceAccountHandlePrefix = "service_account:"

var serviceAccountName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// CreateServiceAccountInput creates a service account. Service accounts have
// no password; they authenticate with API keys.
type CreateServiceAccountInput struct {
	Name, Description, GlobalRole string
}

// CreateServiceAccount creates a service account.
func (s *Service) CreateServiceAccount(ctx context.Context, actor auth.Principal, in CreateServiceAccountInput, requestID string) (ServiceAccount, error) {
	if err := s.authorize(actor, audit.ActionServiceAccountCreated, "service_account", "", requestID); err != nil {
		return ServiceAccount{}, err
	}
	if !serviceAccountName.MatchString(in.Name) {
		return ServiceAccount{}, invalid("name must be 1-63 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit")
	}
	if len(in.Description) > 1000 {
		return ServiceAccount{}, invalid("description must be at most 1000 characters")
	}
	if in.GlobalRole == "" {
		in.GlobalRole = string(authz.Member)
	}
	role, err := validRole(in.GlobalRole)
	if err != nil {
		return ServiceAccount{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return ServiceAccount{}, fmt.Errorf("admin: principal id: %w", err)
	}
	now := s.nowUTC()
	err = s.inTx(ctx, func(tx pgx.Tx, q *admindb.Queries) error {
		if err := q.CreatePrincipal(ctx, admindb.CreatePrincipalParams{
			ID: id, Kind: admindb.StudioActorKindServiceAccount, Handle: ServiceAccountHandlePrefix + in.Name, CreatedBy: actor.ID,
		}); err != nil {
			return fmt.Errorf("admin: create principal: %w", err)
		}
		if err := q.CreateServiceAccount(ctx, admindb.CreateServiceAccountParams{
			PrincipalID: id, Description: in.Description, GlobalRole: string(role), CreatedAt: now, CreatedBy: actor.ID,
		}); err != nil {
			return fmt.Errorf("admin: create service account: %w", err)
		}
		return s.record(ctx, tx, actor, audit.ActionServiceAccountCreated, "service_account", id.String(), requestID,
			audit.ServiceAccountCreatedDetails{Name: in.Name, GlobalRole: string(role)})
	})
	if err != nil {
		if isUniqueViolation(err) {
			return ServiceAccount{}, fmt.Errorf("%w: a service account with this name already exists", ErrConflict)
		}
		return ServiceAccount{}, err
	}
	return ServiceAccount{ID: id, Name: in.Name, Description: in.Description, GlobalRole: string(role),
		Status: "active", CreatedAt: now}, nil
}

// ListServiceAccounts returns one page of service accounts.
func (s *Service) ListServiceAccounts(ctx context.Context, actor auth.Principal, page Page, requestID string) ([]ServiceAccount, string, error) {
	if err := s.authorize(actor, audit.ActionServiceAccountList, "service_account", "", requestID); err != nil {
		return nil, "", err
	}
	after, limit, err := page.decode()
	if err != nil {
		return nil, "", err
	}
	rows, err := s.q.ListServiceAccounts(ctx, admindb.ListServiceAccountsParams{After: after, RowLimit: limit})
	if err != nil {
		return nil, "", fmt.Errorf("admin: list service accounts: %w", err)
	}
	out := make([]ServiceAccount, len(rows))
	for i, r := range rows {
		out[i] = ServiceAccount{ID: r.PrincipalID, Description: r.Description, GlobalRole: r.GlobalRole,
			Status: r.Status, CreatedAt: r.CreatedAt}
	}
	next := ""
	if len(out) > 0 {
		next = nextCursor(len(out), int(limit), out[len(out)-1].ID)
	}
	return out, next, nil
}

// APIKey describes a key without its secret or hash.
type APIKey struct {
	ID        uuid.UUID
	Prefix    string
	CreatedAt time.Time
	CreatedBy uuid.UUID
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// IssueAPIKey creates a key for a user or service account. The secret is
// returned once and never stored.
func (s *Service) IssueAPIKey(ctx context.Context, actor auth.Principal, principalID uuid.UUID, expiresAt time.Time, requestID string) (auth.IssuedAPIKey, error) {
	if err := s.authorize(actor, audit.ActionAPIKeyCreated, "principal", principalID.String(), requestID); err != nil {
		return auth.IssuedAPIKey{}, err
	}
	if expiresAt.IsZero() {
		return auth.IssuedAPIKey{}, invalid("expires_at is required, as an RFC 3339 time")
	}
	k, err := s.auth.IssueAPIKey(ctx, actor, principalID, expiresAt, requestID)
	if errors.Is(err, auth.ErrNotFound) {
		return auth.IssuedAPIKey{}, ErrNotFound
	}
	return k, err
}

// ListAPIKeys lists a principal's keys, newest last.
func (s *Service) ListAPIKeys(ctx context.Context, actor auth.Principal, principalID uuid.UUID, requestID string) ([]APIKey, error) {
	if err := s.authorize(actor, audit.ActionAPIKeyList, "principal", principalID.String(), requestID); err != nil {
		return nil, err
	}
	if _, err := s.q.GetPrincipalKind(ctx, principalID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("admin: principal lookup: %w", err)
	}
	rows, err := s.q.ListAPIKeys(ctx, principalID)
	if err != nil {
		return nil, fmt.Errorf("admin: list api keys: %w", err)
	}
	out := make([]APIKey, len(rows))
	for i, r := range rows {
		out[i] = APIKey(r)
	}
	return out, nil
}

// RevokeAPIKey permanently revokes a key.
func (s *Service) RevokeAPIKey(ctx context.Context, actor auth.Principal, keyID uuid.UUID, requestID string) error {
	if err := s.authorize(actor, audit.ActionAPIKeyRevoked, "api_key", keyID.String(), requestID); err != nil {
		return err
	}
	err := s.auth.RevokeAPIKey(ctx, actor, keyID, requestID)
	if errors.Is(err, auth.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
