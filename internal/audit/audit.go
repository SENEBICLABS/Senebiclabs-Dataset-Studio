// Package audit writes audit events (plan section 8).
//
// Two paths exist:
//
//   - Writer.Record writes one succeeded event inside the caller's
//     transaction. One command is one transaction with exactly one succeeded
//     event; if the transaction rolls back, so does the event.
//   - Aggregator collects denied and failed events in memory and writes one
//     aggregated event per key per window, out of band (plan 8.3).
//
// audit_events is append-only. Nothing in this package updates or deletes.
// The database enforces the structural rules (actor kind, anonymous actor,
// on_behalf_of, job execution and causation); this package fails early on the
// same rules so callers get clear errors, and never relies on them alone.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Outcome of an audited action.
type Outcome string

// Outcomes (plan 5.2).
const (
	Succeeded Outcome = "succeeded"
	Denied    Outcome = "denied"
	Failed    Outcome = "failed"
)

// Class of an audited action (plan 5.2).
type Class string

// Action classes.
const (
	Command      Class = "command"
	JobExecution Class = "job_execution"
	Security     Class = "security"
)

// ActorKind mirrors the studio.actor_kind enum.
type ActorKind string

// Actor kinds.
const (
	User           ActorKind = "user"
	ServiceAccount ActorKind = "service_account"
	System         ActorKind = "system"
	Anonymous      ActorKind = "anonymous"
)

// JobTrigger says why a job ran (plan 8.2).
type JobTrigger string

// Job triggers.
const (
	TriggerCommand  JobTrigger = "command"
	TriggerPeriodic JobTrigger = "periodic"
)

// Job identifies the River job execution an event records.
type Job struct {
	ID      int64
	Attempt int
	Trigger JobTrigger
}

// Event is one audit event. Server-assigned fields (id is generated here;
// occurred_at and tx_id are assigned by the database) are not set by callers.
type Event struct {
	Action           string
	Outcome          Outcome
	ActorPrincipalID string // empty only for anonymous actors
	ActorKind        ActorKind
	OnBehalfOf       string // system actors only
	AuthMethod       string
	TargetType       string
	TargetID         string
	ProjectID        string
	DatasetID        string
	RequestID        string
	Job              *Job
	CausationEventID string
	// Details is the action's details struct (see Catalog). It is encoded as
	// a JSON object.
	Details any
}

// Execer is satisfied by pgx.Tx, *pgx.Conn and *pgxpool.Pool.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Writer records succeeded events inside the caller's transaction.
type Writer struct {
	catalog Catalog
	newID   func() (uuid.UUID, error)
}

// NewWriter returns a Writer that accepts only actions in catalog.
func NewWriter(catalog Catalog) *Writer {
	return &Writer{catalog: catalog, newID: uuid.NewV7}
}

// Record inserts ev, which must have outcome Succeeded, using tx. Pass the
// command's own transaction so the event commits or rolls back with it.
// It returns the new event id.
func (w *Writer) Record(ctx context.Context, tx Execer, ev Event) (string, error) {
	if ev.Outcome != Succeeded {
		return "", fmt.Errorf("audit: Record writes succeeded events only; %s events go through the Aggregator", ev.Outcome)
	}
	spec, err := w.catalog.validate(ev)
	if err != nil {
		return "", err
	}
	id, err := w.newID()
	if err != nil {
		return "", fmt.Errorf("audit: generate id: %w", err)
	}
	details, err := encodeDetails(ev.Details)
	if err != nil {
		return "", err
	}
	if err := insert(ctx, tx, id.String(), spec, ev, details); err != nil {
		return "", err
	}
	return id.String(), nil
}

func encodeDetails(d any) ([]byte, error) {
	if d == nil {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("audit: encode details: %w", err)
	}
	if len(b) == 0 || b[0] != '{' {
		return nil, errors.New("audit: details must encode as a JSON object")
	}
	return b, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const insertSQL = `
INSERT INTO studio.audit_events (
    id, action, action_class, outcome,
    actor_principal_id, actor_kind, on_behalf_of_principal_id, auth_method,
    target_type, target_id, project_id, dataset_id, request_id,
    job_id, job_attempt, job_trigger,
    causation_event_id, causation_outcome,
    details, details_version)
VALUES ($1, $2, $3, $4, $5, $6::studio.actor_kind, $7, $8, $9, $10, $11, $12, $13,
        $14, $15, $16, $17, $18, $19, $20)`

// insert writes one row. It deliberately has no RETURNING clause: the id is
// generated here, so roles with INSERT but no SELECT (studio_worker) can use
// it.
func insert(ctx context.Context, q Execer, id string, spec ActionSpec, ev Event, details []byte) error {
	var jobID, jobAttempt, jobTrigger any
	if ev.Job != nil {
		jobID, jobAttempt, jobTrigger = ev.Job.ID, ev.Job.Attempt, string(ev.Job.Trigger)
	}
	var causationOutcome any
	if ev.CausationEventID != "" {
		causationOutcome = string(Succeeded)
	}
	_, err := q.Exec(ctx, insertSQL,
		id, ev.Action, string(spec.Class), string(ev.Outcome),
		nullable(ev.ActorPrincipalID), string(ev.ActorKind), nullable(ev.OnBehalfOf), nullable(ev.AuthMethod),
		nullable(ev.TargetType), nullable(ev.TargetID), nullable(ev.ProjectID), nullable(ev.DatasetID), nullable(ev.RequestID),
		jobID, jobAttempt, jobTrigger,
		nullable(ev.CausationEventID), causationOutcome,
		details, spec.DetailsVersion,
	)
	if err != nil {
		return fmt.Errorf("audit: insert %s: %w", ev.Action, err)
	}
	return nil
}
