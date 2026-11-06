// Package jobs runs background jobs (plan 3.2, 3.3 and 12).
//
// River v0.49.0 is the queue, kept behind this package so it can be replaced
// (plan 3.2: the exit route is Queue plus its conformance suite). Callers see
// only:
//
//   - Queue, which enqueues a job inside the caller's transaction;
//   - Registry and HandlerFunc, which declare job kinds;
//   - Runner, which executes them in studio-worker.
//
// Every handler runs inside the fenced transaction of plan 12.1 (see
// fence.go): the job row is locked and its attempt checked, the handler's
// domain writes and exactly one job_execution audit event are made, and the
// job is completed, all in one transaction. A stale or cancelled attempt
// returns an error, never nil.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
)

// Schema holds River's tables (migration 00005).
const Schema = "studio"

// DefaultQueue is the only queue in Phase 1.
const DefaultQueue = river.QueueDefault

// Defaults from plan 3.3 (R6, R7).
const (
	DefaultJobTimeout           = time.Minute
	DefaultRescueStuckJobsAfter = time.Hour
	CompletedJobRetention       = 30 * 24 * time.Hour
	DefaultConcurrency          = 4
)

// Errors returned by the fence. Each makes River record a failed attempt;
// none is ever swallowed (plan 12.1: never return nil on a stale attempt).
var (
	ErrStaleAttempt = errors.New("jobs: stale attempt: the job is no longer running this attempt")
	ErrCancelled    = errors.New("jobs: cancellation was requested for this job")
	ErrNotCompleted = errors.New("jobs: completion did not leave the job completed")
)

// Args are a job's arguments. Kind names the job kind and must have a value
// receiver; Args are encoded as JSON.
type Args interface {
	Kind() string
}

// Origin records why a job was enqueued (plan 8.2). A job enqueued by a
// command carries that command's succeeded event and the accountable
// principal, and its audit event is command-triggered. A job with a zero
// Origin is periodic and its event carries neither.
type Origin struct {
	CausationEventID string
	OnBehalfOf       string
}

// Trigger is the audit job trigger implied by the origin.
func (o Origin) Trigger() audit.JobTrigger {
	if o.CausationEventID != "" {
		return audit.TriggerCommand
	}
	return audit.TriggerPeriodic
}

// Validate rejects a half-set origin: plan 8.2 requires causation and
// on_behalf_of together for command-triggered jobs.
func (o Origin) Validate() error {
	if (o.CausationEventID == "") != (o.OnBehalfOf == "") {
		return errors.New("jobs: origin needs both a causation event and an accountable principal, or neither")
	}
	return nil
}

// Queue enqueues jobs. InsertTx adds the job inside tx: workers cannot see it
// until tx commits, and it never runs if tx rolls back (plan 3.3 R5, test
// J6). It returns the job id, which the enqueuing command lists in its audit
// event's details.enqueued_jobs (plan 8.2).
type Queue interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args Args, origin Origin) (int64, error)
}

// JobRef identifies one attempt of one job.
type JobRef struct {
	ID      int64
	Attempt int
}

// Job is what a handler receives.
type Job[A Args] struct {
	JobRef
	Args   A
	Origin Origin
}

// Result describes the audited effect of one job execution. The fence writes
// it as the job's single job_execution event; handlers never write audit
// events themselves.
type Result struct {
	TargetType string
	TargetID   string
	ProjectID  string
	DatasetID  string
	// Details must encode as a JSON object. A job that enqueues further jobs
	// lists them here under enqueued_jobs (plan 8.2).
	Details any
}

// HandlerFunc performs one job's domain writes in tx, the fenced
// transaction. It must be idempotent (plan 12.1): running the same work twice
// leaves the same domain state. It must not commit or roll back tx.
type HandlerFunc[A Args] func(ctx context.Context, tx pgx.Tx, job Job[A]) (Result, error)

// KindOptions configures one job kind.
type KindOptions struct {
	// Action is the job_execution audit action the kind writes. It must be
	// in the audit catalog with class job_execution.
	Action string
	// Timeout overrides the runner's job timeout for this kind (0 keeps it).
	Timeout time.Duration
	// Every, when positive, makes the kind periodic: the elected leader
	// enqueues it with zero arguments and no origin at this interval, so its
	// audit events are periodic (plan 8.2).
	Every time.Duration
}

type kindSpec struct {
	action   string
	add      func(w *river.Workers, f *fence)
	periodic *river.PeriodicJob
}

// Registry declares job kinds. It is a plain description; a Runner turns it
// into River workers.
type Registry struct {
	kinds map[string]kindSpec
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{kinds: map[string]kindSpec{}}
}

// Register adds the job kind A with handler h.
func Register[A Args](r *Registry, h HandlerFunc[A], opts KindOptions) error {
	var zero A
	kind := zero.Kind()
	switch {
	case kind == "":
		return errors.New("jobs: a job kind must have a name")
	case h == nil:
		return fmt.Errorf("jobs: kind %s has no handler", kind)
	case opts.Action == "":
		return fmt.Errorf("jobs: kind %s has no audit action", kind)
	case opts.Timeout < 0:
		return fmt.Errorf("jobs: kind %s has a negative timeout", kind)
	case opts.Every < 0:
		return fmt.Errorf("jobs: kind %s has a negative period", kind)
	}
	if _, dup := r.kinds[kind]; dup {
		return fmt.Errorf("jobs: kind %s is registered twice", kind)
	}
	spec := kindSpec{
		action: opts.Action,
		add: func(w *river.Workers, f *fence) {
			river.AddWorker(w, &riverWorker[A]{fence: f, handler: h, action: opts.Action, timeout: opts.Timeout})
		},
	}
	if opts.Every > 0 {
		spec.periodic = river.NewPeriodicJob(river.PeriodicInterval(opts.Every),
			func() (river.JobArgs, *river.InsertOpts) { return zero, &river.InsertOpts{Queue: DefaultQueue} },
			&river.PeriodicJobOpts{ID: kind})
	}
	r.kinds[kind] = spec
	return nil
}

// Periodic reports whether a registered kind is periodic.
func (r *Registry) Periodic(kind string) bool {
	s, ok := r.kinds[kind]
	return ok && s.periodic != nil
}

// Kinds returns the registered kinds, sorted.
func (r *Registry) Kinds() []string {
	out := make([]string, 0, len(r.kinds))
	for k := range r.kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Action returns the audit action a registered kind writes.
func (r *Registry) Action(kind string) (string, bool) {
	s, ok := r.kinds[kind]
	return s.action, ok
}

// CheckActions verifies that every kind's audit action is a job_execution
// action in catalog. This is the job half of the audit-completeness rule
// (plan 8.1): every job kind writes exactly one event, of a cataloged action.
func (r *Registry) CheckActions(catalog audit.Catalog) error {
	var errs []error
	for _, kind := range r.Kinds() {
		spec, ok := catalog[r.kinds[kind].action]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("kind %s: action %q is not in the audit catalog", kind, r.kinds[kind].action))
		case spec.Class != audit.JobExecution:
			errs = append(errs, fmt.Errorf("kind %s: action %q has class %s, want %s", kind, r.kinds[kind].action, spec.Class, audit.JobExecution))
		}
	}
	return errors.Join(errs...)
}
