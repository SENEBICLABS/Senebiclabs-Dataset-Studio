package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
)

// Metadata keys under which a job carries its origin. River owns the "river:"
// prefix; ours never collide with it.
const (
	metaCausation  = "studio:causation_event_id"
	metaOnBehalfOf = "studio:on_behalf_of"
)

func originMetadata(o Origin) ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if o.CausationEventID == "" {
		return nil, nil
	}
	b, err := json.Marshal(map[string]string{metaCausation: o.CausationEventID, metaOnBehalfOf: o.OnBehalfOf})
	if err != nil {
		return nil, fmt.Errorf("jobs: encode origin: %w", err)
	}
	return b, nil
}

func originFromMetadata(raw []byte) Origin {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return Origin{}
	}
	causation, _ := m[metaCausation].(string)
	onBehalfOf, _ := m[metaOnBehalfOf].(string)
	return Origin{CausationEventID: causation, OnBehalfOf: onBehalfOf}
}

// riverQueue implements Queue with a River client.
type riverQueue struct {
	client *river.Client[pgx.Tx]
}

// NewQueue returns an insert-only Queue for a pool connected as a role that
// holds the River insert grants (studio_app, plan 6.3 R5).
func NewQueue(pool *pgxpool.Pool, log *slog.Logger) (Queue, error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Schema: Schema,
		Logger: log,
	})
	if err != nil {
		return nil, fmt.Errorf("jobs: queue client: %w", err)
	}
	return riverQueue{client: client}, nil
}

func (q riverQueue) InsertTx(ctx context.Context, tx pgx.Tx, args Args, origin Origin) (int64, error) {
	return insertTx(ctx, q.client, tx, args, origin)
}

func insertTx(ctx context.Context, client *river.Client[pgx.Tx], tx pgx.Tx, args Args, origin Origin) (int64, error) {
	meta, err := originMetadata(origin)
	if err != nil {
		return 0, err
	}
	res, err := client.InsertTx(ctx, tx, argsAdapter{args}, &river.InsertOpts{Metadata: meta, Queue: DefaultQueue})
	if err != nil {
		return 0, fmt.Errorf("jobs: insert %s: %w", args.Kind(), err)
	}
	return res.Job.ID, nil
}

// argsAdapter lets a jobs.Args value pass through River's insert, which
// needs river.JobArgs; the method sets are identical. It encodes as the
// wrapped value.
type argsAdapter struct{ Args }

func (a argsAdapter) MarshalJSON() ([]byte, error) { return json.Marshal(a.Args) }

// riverWorker adapts a HandlerFunc to River and runs it inside the fence.
type riverWorker[A Args] struct {
	river.WorkerDefaults[A]
	fence   *fence
	handler HandlerFunc[A]
	action  string
	timeout time.Duration
}

func (w *riverWorker[A]) Timeout(*river.Job[A]) time.Duration { return w.timeout }

func (w *riverWorker[A]) Work(ctx context.Context, job *river.Job[A]) error {
	return run(ctx, w.fence, job, w.handler, w.action)
}

// RetryPolicy returns the delay before the next attempt, given the attempt
// that just failed. nil keeps River's default exponential backoff.
type RetryPolicy func(attempt int) time.Duration

type retryPolicy struct{ f RetryPolicy }

func (p retryPolicy) NextRetry(job *rivertype.JobRow) time.Time {
	return time.Now().Add(p.f(job.Attempt))
}

// RunnerConfig configures studio-worker's job runner.
type RunnerConfig struct {
	// Pool must be connected as studio_worker (plan 6.1).
	Pool     *pgxpool.Pool
	Registry *Registry
	// Catalog is the audit catalog the fence writes against; nil means
	// audit.Actions.
	Catalog audit.Catalog
	Logger  *slog.Logger

	// ID names this client; empty lets River choose.
	ID                   string
	Concurrency          int           // 0 means DefaultConcurrency
	JobTimeout           time.Duration // 0 means DefaultJobTimeout (R6)
	RescueStuckJobsAfter time.Duration // 0 means DefaultRescueStuckJobsAfter (R6)
	FetchPollInterval    time.Duration // 0 keeps River's default
	RetryPolicy          RetryPolicy   // nil keeps River's default
	Hooks                Hooks
}

// Runner executes registered job kinds.
type Runner struct {
	client *river.Client[pgx.Tx]
}

// NewRunner builds a Runner. It fails if the registry is empty or a kind's
// audit action is not a cataloged job_execution action.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if cfg.Pool == nil || cfg.Registry == nil || cfg.Logger == nil {
		return nil, errors.New("jobs: runner needs a pool, a registry and a logger")
	}
	if len(cfg.Registry.kinds) == 0 {
		return nil, errors.New("jobs: no job kinds are registered")
	}
	catalog := cfg.Catalog
	if catalog == nil {
		catalog = audit.Actions
	}
	if err := cfg.Registry.CheckActions(catalog); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	f := &fence{pool: cfg.Pool, writer: audit.NewWriter(catalog), hooks: cfg.Hooks}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	workers := river.NewWorkers()
	for _, kind := range cfg.Registry.Kinds() {
		cfg.Registry.kinds[kind].add(workers, f)
	}
	rc := &river.Config{
		ID:                          cfg.ID,
		Schema:                      Schema,
		Logger:                      cfg.Logger,
		Workers:                     workers,
		Queues:                      map[string]river.QueueConfig{DefaultQueue: {MaxWorkers: concurrency}},
		JobTimeout:                  durationOr(cfg.JobTimeout, DefaultJobTimeout),
		RescueStuckJobsAfter:        durationOr(cfg.RescueStuckJobsAfter, DefaultRescueStuckJobsAfter),
		CompletedJobRetentionPeriod: CompletedJobRetention,
		FetchPollInterval:           cfg.FetchPollInterval,
	}
	if cfg.RetryPolicy != nil {
		rc.RetryPolicy = retryPolicy{f: cfg.RetryPolicy}
	}
	client, err := river.NewClient(riverpgxv5.New(cfg.Pool), rc)
	if err != nil {
		return nil, fmt.Errorf("jobs: runner client: %w", err)
	}
	return &Runner{client: client}, nil
}

func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Start begins fetching and working jobs, and River's maintenance (rescuer,
// cleaner, scheduler, reindexer) when this client is elected leader.
func (r *Runner) Start(ctx context.Context) error {
	if err := r.client.Start(ctx); err != nil {
		return fmt.Errorf("jobs: start: %w", err)
	}
	return nil
}

// Stop stops fetching and waits for running jobs to finish or ctx to end.
func (r *Runner) Stop(ctx context.Context) error {
	if err := r.client.Stop(ctx); err != nil {
		return fmt.Errorf("jobs: stop: %w", err)
	}
	return nil
}

// StopAndCancel stops fetching and cancels running jobs' contexts.
func (r *Runner) StopAndCancel(ctx context.Context) error {
	if err := r.client.StopAndCancel(ctx); err != nil {
		return fmt.Errorf("jobs: stop and cancel: %w", err)
	}
	return nil
}

// Shutdown stops the runner for process shutdown: it waits for running jobs
// until ctx ends and, if they have not finished by then, cancels them and
// waits a further grace period, so no job outlives the pool it uses. A
// cancelled job's fenced transaction rolls back and River retries it.
func (r *Runner) Shutdown(ctx context.Context, grace time.Duration) error {
	err := r.Stop(ctx)
	if err == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	if cerr := r.StopAndCancel(cctx); cerr != nil {
		return errors.Join(err, cerr)
	}
	return err
}

// Queue returns a Queue on this runner's client, for jobs that enqueue
// further jobs in their fenced transaction.
func (r *Runner) Queue() Queue { return runnerQueue{r.client} }

type runnerQueue struct{ client *river.Client[pgx.Tx] }

func (q runnerQueue) InsertTx(ctx context.Context, tx pgx.Tx, args Args, origin Origin) (int64, error) {
	return insertTx(ctx, q.client, tx, args, origin)
}
