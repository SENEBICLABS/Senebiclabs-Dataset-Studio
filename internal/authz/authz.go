// Package authz decides whether a principal may perform an operation (plan
// section 14, "Authorization").
//
//   - Deny by default: a permission that is not in the matrix, a role that is
//     not listed for it, or a scope the principal does not hold is denied.
//   - The role-to-permission mapping lives in code (Matrix) and is the only
//     place roles are interpreted.
//   - Services call Authorizer.Authorize before they touch the database, so
//     the check runs in the service layer (CLAUDE.md section 3) and a denied
//     request learns nothing about the resource. Each denial is recorded as a
//     denied audit event through the aggregator (plan 8.1, 8.3).
//
// Tenancy (plan 2.2, Assumption T1): Principal carries a Scopes slice that
// holds one implicit installation scope today, and every check takes a Target
// naming its scope, so a later workspace scope needs no new signature.
package authz

import (
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
)

// ErrDenied is returned when the principal lacks the permission. Handlers map
// it to 403.
var ErrDenied = errors.New("authz: permission denied")

// ErrDenialNotRecorded accompanies ErrDenied when the denied event could not
// be handed to the recorder. The request is refused either way.
var ErrDenialNotRecorded = errors.New("authz: denied event not recorded")

// GlobalRole is users.global_role or service_accounts.global_role.
type GlobalRole string

// Global roles (migration 00001 CHECK constraints).
const (
	PlatformAdmin GlobalRole = "platform_admin"
	RegistryAdmin GlobalRole = "registry_admin"
	Member        GlobalRole = "member"
)

// ValidGlobalRole reports whether r is a known global role.
func ValidGlobalRole(r GlobalRole) bool {
	return r == PlatformAdmin || r == RegistryAdmin || r == Member
}

// ProjectRole is a principal's role in one project. Project membership is
// stored from M10 (project_members); until then no caller can supply one.
type ProjectRole string

// Project roles (plan 14).
const (
	Owner       ProjectRole = "owner"
	Maintainer  ProjectRole = "maintainer"
	Contributor ProjectRole = "contributor"
	Viewer      ProjectRole = "viewer"
)

// Scope is a tenancy scope (plan 2.2). Phase 1 has exactly one.
type Scope string

// InstallationScope is the single implicit scope of a Phase 1 installation.
const InstallationScope Scope = "installation"

// Principal is the subject of an authorization decision.
type Principal struct {
	ID         uuid.UUID
	Kind       audit.ActorKind
	GlobalRole GlobalRole
	// AuthMethod is recorded on denied events.
	AuthMethod string
	// Scopes holds InstallationScope for every Phase 1 principal.
	Scopes []Scope
}

// Target says where an operation applies. The zero value is invalid: build
// one with Global or InProject.
type Target struct {
	Scope     Scope
	ProjectID uuid.UUID // uuid.Nil for installation-wide operations
	// ProjectRole is the principal's role in ProjectID, resolved by the
	// caller from project membership; empty when not a member.
	ProjectRole ProjectRole
}

// Global targets an installation-wide operation.
func Global() Target { return Target{Scope: InstallationScope} }

// InProject targets an operation inside project id, where the principal holds
// role (empty when not a member).
func InProject(id uuid.UUID, role ProjectRole) Target {
	return Target{Scope: InstallationScope, ProjectID: id, ProjectRole: role}
}

// Allowed is the pure decision: true only when the matrix grants perm to the
// principal's global role, or to its role in the target project.
func Allowed(p Principal, perm Permission, t Target) bool {
	rule, ok := Matrix[perm]
	if !ok || t.Scope == "" || !slices.Contains(p.Scopes, t.Scope) {
		return false
	}
	switch p.Kind {
	case audit.User, audit.ServiceAccount:
	default:
		// System principals cannot authenticate (plan 7.1) and anonymous
		// callers never reach a check.
		return false
	}
	if slices.Contains(rule.Global, p.GlobalRole) {
		return true
	}
	if rule.ProjectScoped && t.ProjectID != uuid.Nil && t.ProjectRole != "" {
		return slices.Contains(rule.Project, t.ProjectRole)
	}
	return false
}

// Recorder receives denied events (the audit Aggregator).
type Recorder interface {
	Add(ev audit.Event) error
}

// Denial describes the refused operation for its audit event.
type Denial struct {
	// Action is the catalog action the request attempted (plan 8.3 groups
	// denied events by actor, action, target and outcome).
	Action     string
	TargetType string
	TargetID   string
	RequestID  string
}

// Authorizer checks permissions and records denials.
type Authorizer struct {
	rec Recorder
}

// NewAuthorizer returns an Authorizer that records denials in rec.
func NewAuthorizer(rec Recorder) *Authorizer { return &Authorizer{rec: rec} }

// Authorize returns nil when p holds perm on t. Otherwise it records one
// denied event and returns an error wrapping ErrDenied. A denial that cannot
// be recorded is still a denial: the error then also carries the recording
// failure, and the request must be refused either way.
func (a *Authorizer) Authorize(p Principal, perm Permission, t Target, d Denial) error {
	if Allowed(p, perm, t) {
		return nil
	}
	ev := audit.Event{
		Action: d.Action, Outcome: audit.Denied,
		ActorPrincipalID: p.ID.String(), ActorKind: p.Kind, AuthMethod: p.AuthMethod,
		TargetType: d.TargetType, TargetID: d.TargetID, RequestID: d.RequestID,
		Details: audit.DeniedDetails{Permission: string(perm)},
	}
	if t.ProjectID != uuid.Nil {
		ev.ProjectID = t.ProjectID.String()
	}
	if err := a.rec.Add(ev); err != nil {
		return fmt.Errorf("%w: %w: %w", ErrDenied, ErrDenialNotRecorded, err)
	}
	return ErrDenied
}
