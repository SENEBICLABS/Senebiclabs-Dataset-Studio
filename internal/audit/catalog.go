package audit

import (
	"errors"
	"fmt"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals"
)

// ActionSpec describes one auditable action: its class and the version of its
// details struct. Bump DetailsVersion whenever the details struct changes.
type ActionSpec struct {
	Class          Class
	DetailsVersion int
}

// Catalog maps action names to their specs. Every action written must be in
// the catalog (plan 8.1); an unknown action is an error, never a silent write.
type Catalog map[string]ActionSpec

// Actions known to the application. Each milestone adds the actions it
// introduces, together with a details struct.
var Actions = Catalog{
	// Written once by migration 00003 for the rows seeded by migration 00002
	// (plan 8.1). Listed so readers can decode it.
	ActionSystemSeeded: {Class: Command, DetailsVersion: 1},

	// Authentication (M3, plan 8.1: login, logout, session and key changes
	// are normal events; a failed login is a failed event).
	ActionLogin:         {Class: Security, DetailsVersion: 1},
	ActionLoginFailed:   {Class: Security, DetailsVersion: 1},
	ActionLogout:        {Class: Security, DetailsVersion: 1},
	ActionAPIKeyCreated: {Class: Security, DetailsVersion: 1},
	ActionAPIKeyRevoked: {Class: Security, DetailsVersion: 1},
	ActionUserCreated:   {Class: Command, DetailsVersion: 1},

	// User and service-account administration (M4). Role, password and
	// status changes are security events (plan 8.1).
	ActionUserUpdated:           {Class: Security, DetailsVersion: 1},
	ActionUserDisabled:          {Class: Security, DetailsVersion: 1},
	ActionServiceAccountCreated: {Class: Command, DetailsVersion: 1},

	// Reads are not audited (plan 8.1, U3), but a refused read is: every 403
	// is a denied event (plan 8.1). These actions are written only with
	// outcome denied, through the aggregator.
	ActionUserList:           {Class: Security, DetailsVersion: 1},
	ActionUserRead:           {Class: Security, DetailsVersion: 1},
	ActionServiceAccountList: {Class: Security, DetailsVersion: 1},
	ActionAPIKeyList:         {Class: Security, DetailsVersion: 1},

	// Blob store jobs (M5, plan 8.1 and 11): one blob.pending_swept event per
	// sweep, one blob.audited event per audit run. Details structs are
	// blob.SweepDetails and blob.AuditDetails.
	ActionBlobPendingSwept: {Class: JobExecution, DetailsVersion: 1},
	ActionBlobAudited:      {Class: JobExecution, DetailsVersion: 1},

	// Restore (M7, plan 8.1 and 13): one event after verification, written by
	// studioctl as system:cli.
	ActionSystemRestored: {Class: Command, DetailsVersion: 1},
}

// Action names.
const (
	ActionSystemSeeded = "system.seeded"
	// ActionLoginFailed is the only action an anonymous actor may have (plan
	// 7.1).
	ActionLoginFailed   = "auth.login_failed"
	ActionLogin         = "auth.login"
	ActionLogout        = "auth.logout"
	ActionAPIKeyCreated = "auth.api_key_created" //nolint:gosec // G101: an audit action name, not a credential.
	ActionAPIKeyRevoked = "auth.api_key_revoked" //nolint:gosec // G101: an audit action name, not a credential.
	ActionUserCreated   = "user.created"

	ActionUserUpdated           = "user.updated"
	ActionUserDisabled          = "user.disabled"
	ActionServiceAccountCreated = "service_account.created"
	ActionUserList              = "user.list"
	ActionUserRead              = "user.read"
	ActionServiceAccountList    = "service_account.list"
	ActionAPIKeyList            = "auth.api_key_list" //nolint:gosec // G101: an audit action name, not a credential.
	ActionBlobPendingSwept      = "blob.pending_swept"
	ActionBlobAudited           = "blob.audited"
	ActionSystemRestored        = "system.restored"
)

// SystemRestoredDetails is the details struct of system.restored, version 1.
// OSUser and Hostname identify the operator (plan 7.2).
type SystemRestoredDetails struct {
	BackupCreatedAt string `json:"backup_created_at"`
	ManifestSHA256  string `json:"manifest_sha256"`
	InstallationID  string `json:"installation_id"`
	GooseVersion    int64  `json:"goose_version"`
	Blobs           int    `json:"blobs"`
	OSUser          string `json:"os_user,omitempty"`
	Hostname        string `json:"hostname,omitempty"`
}

// DeniedDetails is the details struct of every denied event, version 1. The
// aggregator adds repeat_count, first_seen and last_seen (plan 8.3).
type DeniedDetails struct {
	Permission string `json:"permission"`
}

// UserUpdatedDetails is the details struct of user.updated, version 1. It
// names the changed fields and never carries a password or its hash.
type UserUpdatedDetails struct {
	Fields          []string `json:"fields"`
	GlobalRole      string   `json:"global_role,omitempty"`
	SessionsRevoked int64    `json:"sessions_revoked"`
}

// UserDisabledDetails is the details struct of user.disabled, version 1.
type UserDisabledDetails struct {
	SessionsRevoked int64 `json:"sessions_revoked"`
}

// ServiceAccountCreatedDetails is the details struct of
// service_account.created, version 1.
type ServiceAccountCreatedDetails struct {
	Name       string `json:"name"`
	GlobalRole string `json:"global_role"`
}

// LoginDetails is the details struct of auth.login, version 1.
type LoginDetails struct {
	SessionExpiresAt string `json:"session_expires_at"`
}

// LoginFailedDetails is the details struct of auth.login_failed, version 1.
// IdentifierHMAC is set for anonymous attempts (unknown identifier); the raw
// identifier is never recorded (plan 8.1).
type LoginFailedDetails struct {
	Reason         string `json:"reason"`
	IdentifierHMAC string `json:"identifier_hmac,omitempty"`
}

// LogoutDetails is the details struct of auth.logout, version 1.
type LogoutDetails struct{}

// APIKeyDetails is the details struct of auth.api_key_created and
// auth.api_key_revoked, version 1. The key itself is never recorded.
type APIKeyDetails struct {
	Prefix      string `json:"prefix"`
	PrincipalID string `json:"principal_id"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

// UserCreatedDetails is the details struct of user.created, version 1.
// OSUser and Hostname identify the operator when created by studioctl
// (plan 7.2).
type UserCreatedDetails struct {
	GlobalRole string `json:"global_role"`
	OSUser     string `json:"os_user,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
}

// SystemSeededDetails is the details struct of system.seeded, version 1.
type SystemSeededDetails struct {
	Principals   []string `json:"principals"`
	Installation bool     `json:"installation"`
	Migration    int      `json:"migration"`
}

// validate checks ev against the catalog and the plan's structural rules. The
// database enforces the same rules; checking here gives callers clear errors.
func (c Catalog) validate(ev Event) (ActionSpec, error) {
	spec, ok := c[ev.Action]
	if !ok {
		return ActionSpec{}, fmt.Errorf("audit: action %q is not in the catalog", ev.Action)
	}
	if spec.DetailsVersion < 1 {
		return ActionSpec{}, fmt.Errorf("audit: action %q has no details version", ev.Action)
	}
	var errs []error
	switch ev.Outcome {
	case Succeeded, Denied, Failed:
	default:
		errs = append(errs, fmt.Errorf("outcome %q is not valid", ev.Outcome))
	}
	switch ev.ActorKind {
	case User, ServiceAccount, System:
		if ev.ActorPrincipalID == "" {
			errs = append(errs, fmt.Errorf("actor kind %s requires an actor principal", ev.ActorKind))
		}
	case Anonymous:
		if ev.ActorPrincipalID != "" {
			errs = append(errs, errors.New("an anonymous actor has no principal"))
		}
		if ev.Action != ActionLoginFailed {
			errs = append(errs, fmt.Errorf("an anonymous actor is only allowed for %s", ActionLoginFailed))
		}
	default:
		errs = append(errs, fmt.Errorf("actor kind %q is not valid", ev.ActorKind))
	}
	if ev.OnBehalfOf != "" && ev.ActorKind != System {
		errs = append(errs, errors.New("on_behalf_of is only allowed for system actors"))
	}
	isJob := spec.Class == JobExecution
	switch {
	case isJob && ev.Job == nil:
		errs = append(errs, errors.New("a job_execution action requires a job"))
	case !isJob && ev.Job != nil:
		errs = append(errs, errors.New("only job_execution actions carry a job"))
	case isJob:
		if ev.ActorPrincipalID != principals.WorkerID {
			errs = append(errs, errors.New("job_execution events are written by system:worker"))
		}
		if ev.Job.Attempt < 1 {
			errs = append(errs, errors.New("job attempt must be at least 1"))
		}
		switch ev.Job.Trigger {
		case TriggerCommand:
			if ev.CausationEventID == "" || ev.OnBehalfOf == "" {
				errs = append(errs, errors.New("a command-triggered job requires causation and on_behalf_of"))
			}
		case TriggerPeriodic:
			if ev.CausationEventID != "" || ev.OnBehalfOf != "" {
				errs = append(errs, errors.New("a periodic job has no causation or on_behalf_of"))
			}
		default:
			errs = append(errs, fmt.Errorf("job trigger %q is not valid", ev.Job.Trigger))
		}
	}
	if !isJob && ev.ActorPrincipalID == principals.WorkerID {
		errs = append(errs, errors.New("system:worker writes only job_execution events"))
	}
	if err := errors.Join(errs...); err != nil {
		return ActionSpec{}, fmt.Errorf("audit: invalid %s event: %w", ev.Action, err)
	}
	return spec, nil
}
