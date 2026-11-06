//go:build integration

package integration

// M6 jobs (plan 12, 16.2): the job conformance suite J1-J10 with the Rev 2.2
// refinements (plan 12.2), G5 (River grants), the River schema check and the
// idempotency harness. These tests are the contract of internal/jobs: a
// replacement for River must pass them (plan 3.2, exit route). J4a, J4b,
// J4c and J5a exercise River itself and would be rewritten for another
// backend.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob/fsstore"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/jobkinds"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/jobs"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/jobs/jobstest"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/testdb"
)

const (
	m6Applied   = "m6.effect_applied"
	m6Requested = "m6.effect_requested"
	m6Kind      = "m6.effect"
)

// m6Catalog is the audit catalog the M6 tests write with. Production job
// actions arrive with the milestones that add job kinds.
var m6Catalog = audit.Catalog{
	m6Applied:   {Class: audit.JobExecution, DetailsVersion: 1},
	m6Requested: {Class: audit.Command, DetailsVersion: 1},
}

// effectArgs is the test job kind: it records one row per execution, so a
// duplicate domain effect is visible as a second row.
type effectArgs struct {
	Note string `json:"note"`
}

func (effectArgs) Kind() string { return m6Kind }

func applyEffect(ctx context.Context, tx pgx.Tx, job jobs.Job[effectArgs]) (jobs.Result, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO studio.m6_effects (job_id, attempt, note) VALUES ($1, $2, $3)`,
		job.ID, job.Attempt, job.Args.Note); err != nil {
		return jobs.Result{}, err
	}
	return jobs.Result{TargetType: "m6_effect", TargetID: fmt.Sprint(job.ID), Details: map[string]any{"note": job.Args.Note}}, nil
}

type jobsEnv struct {
	// calls counts handler invocations per "jobID/attempt", committed or
	// not, so tests can prove a stale attempt never reaches the handler.
	calls sync.Map
	db    *testdb.DB
	admin *pgx.Conn
	app   *pgxpool.Pool
	queue jobs.Queue
}

// newJobsEnv returns a fresh database with user U, the test effects table
// (granted to the API and worker roles) and an API-role queue.
func newJobsEnv(t *testing.T) *jobsEnv {
	t.Helper()
	d, admin := auditDB(t)
	if _, err := admin.Exec(ctx(t), `
		CREATE TABLE studio.m6_effects (job_id bigint NOT NULL, attempt int NOT NULL, note text NOT NULL);
		GRANT SELECT, INSERT ON studio.m6_effects TO studio_worker, studio_app;
		CREATE TABLE studio.m6_notes (note text PRIMARY KEY);
		GRANT SELECT, INSERT ON studio.m6_notes TO studio_worker`); err != nil {
		t.Fatal(err)
	}
	app := appPool(t, d)
	q, err := jobs.NewQueue(app, quietLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	return &jobsEnv{db: d, admin: admin, app: app, queue: q}
}

// countingEffect is applyEffect, recording each invocation.
func (e *jobsEnv) countingEffect(c context.Context, tx pgx.Tx, job jobs.Job[effectArgs]) (jobs.Result, error) {
	n, _ := e.calls.LoadOrStore(job.ID, new(atomic.Int32))
	n.(*atomic.Int32).Add(1)
	return applyEffect(c, tx, job)
}

func (e *jobsEnv) handlerCalls(id int64) int32 {
	n, ok := e.calls.Load(id)
	if !ok {
		return 0
	}
	return n.(*atomic.Int32).Load()
}

func (e *jobsEnv) pool(t *testing.T, role string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(ctx(t), e.db.URL(role))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// logBuffer collects River's log output so tests can assert on job errors.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func quietLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(&logBuffer{}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type runnerOpts struct {
	id       string
	hooks    jobs.Hooks
	rescue   time.Duration
	timeout  time.Duration
	logs     *logBuffer
	register func(r *jobs.Registry) error
	catalog  audit.Catalog // nil means m6Catalog
}

// startRunner starts a worker-role runner for the test kind with one worker
// slot, fast polling and immediate retries.
func (e *jobsEnv) startRunner(t *testing.T, o runnerOpts) *jobs.Runner {
	t.Helper()
	reg := jobs.NewRegistry()
	register := o.register
	if register == nil {
		register = func(r *jobs.Registry) error {
			return jobs.Register(r, e.countingEffect, jobs.KindOptions{Action: m6Applied})
		}
	}
	if err := register(reg); err != nil {
		t.Fatal(err)
	}
	catalog := o.catalog
	if catalog == nil {
		catalog = m6Catalog
	}
	logs := o.logs
	if logs == nil {
		logs = &logBuffer{}
	}
	r, err := jobs.NewRunner(jobs.RunnerConfig{
		Pool:     e.pool(t, dbroles.Worker),
		Registry: reg,
		Catalog:  catalog,
		// Info, because River reports job errors at Info.
		Logger:               slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		ID:                   o.id,
		Concurrency:          1,
		FetchPollInterval:    100 * time.Millisecond,
		RescueStuckJobsAfter: o.rescue,
		JobTimeout:           o.timeout,
		RetryPolicy:          func(int) time.Duration { return 0 },
		DisablePeriodic:      true, // tests enqueue periodic kinds explicitly
		Hooks:                o.hooks,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.WithoutCancel(ctx(t))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = r.StopAndCancel(c)
	})
	return r
}

func stopRunner(t *testing.T, r *jobs.Runner) {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.Stop(c); err != nil {
		t.Fatalf("stop runner: %v", err)
	}
}

// enqueueCommand enqueues one job as user U's command: the job carries the
// command's pre-assigned event id, and the event lists the job in
// details.enqueued_jobs (plan 8.2). It returns the job and event ids.
func (e *jobsEnv) enqueueCommand(t *testing.T, note string) (int64, string) {
	t.Helper()
	w := audit.NewWriter(m6Catalog)
	var jobID int64
	eventID, err := audit.NewEventID()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithTx(ctx(t), e.app, func(tx pgx.Tx) error {
		id, err := e.queue.InsertTx(ctx(t), tx, effectArgs{Note: note}, jobs.Origin{CausationEventID: eventID, OnBehalfOf: userU})
		if err != nil {
			return err
		}
		jobID = id
		_, err = w.Record(ctx(t), tx, audit.Event{
			ID: eventID, Action: m6Requested, Outcome: audit.Succeeded,
			ActorPrincipalID: userU, ActorKind: audit.User,
			Details: map[string]any{"enqueued_jobs": []int64{id}},
		})
		return err
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return jobID, eventID
}

// enqueuePeriodic enqueues a job with no origin, as a periodic job is.
func (e *jobsEnv) enqueuePeriodic(t *testing.T, args jobs.Args) int64 {
	t.Helper()
	var jobID int64
	if err := db.WithTx(ctx(t), e.app, func(tx pgx.Tx) error {
		id, err := e.queue.InsertTx(ctx(t), tx, args, jobs.Origin{})
		jobID = id
		return err
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return jobID
}

type riverJobRow struct {
	state    string
	attempt  int
	errors   int
	metadata map[string]any
}

func (e *jobsEnv) job(t *testing.T, id int64) riverJobRow {
	t.Helper()
	var r riverJobRow
	var meta []byte
	if err := e.admin.QueryRow(ctx(t), `
		SELECT state::text, attempt, coalesce(array_length(errors, 1), 0), metadata
		FROM studio.river_job WHERE id = $1`, id).Scan(&r.state, &r.attempt, &r.errors, &meta); err != nil {
		t.Fatalf("read job %d: %v", id, err)
	}
	if err := json.Unmarshal(meta, &r.metadata); err != nil {
		t.Fatal(err)
	}
	return r
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *jobsEnv) waitState(t *testing.T, id int64, state string, timeout time.Duration) riverJobRow {
	t.Helper()
	var last riverJobRow
	waitFor(t, fmt.Sprintf("job %d to be %s", id, state), timeout, func() bool {
		last = e.job(t, id)
		return last.state == state
	})
	return last
}

func (e *jobsEnv) effects(t *testing.T, id int64) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(ctx(t), `SELECT count(*) FROM studio.m6_effects WHERE job_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *jobsEnv) jobEvents(t *testing.T, id int64) int {
	t.Helper()
	return countEvents(t, e.admin, `action_class = 'job_execution' AND job_id = $1`, id)
}

// gate pauses a hook until released, and reports when it was reached.
type gate struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGate() *gate { return &gate{reached: make(chan struct{}), release: make(chan struct{})} }

func (g *gate) wait(ctx context.Context) {
	g.once.Do(func() { close(g.reached) })
	select {
	case <-g.release:
	case <-ctx.Done():
	}
}

func (g *gate) awaitReached(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// pauseAttempt returns a BeforeFence hook that holds the given attempt at g.
func pauseAttempt(attempt int, g *gate) func(context.Context, jobs.JobRef) {
	return func(c context.Context, ref jobs.JobRef) {
		if ref.Attempt == attempt {
			g.wait(c)
		}
	}
}

func riverExec(p *pgxpool.Pool) riverdriver.Executor { return riverpgxv5.New(p).GetExecutor() }

// rescue runs River's own rescue query (X3: JobRescueMany is exported on
// riverdriver.Executor) with a horizon in the future, so a running job is
// rescued at once, then River's own scheduling query makes it available.
func rescue(t *testing.T, p *pgxpool.Pool, id int64) {
	t.Helper()
	now := time.Now().UTC()
	errData, _ := json.Marshal(rivertype.AttemptError{At: now, Attempt: 1, Error: "Stuck job rescued by JobRescuer"})
	exec := riverExec(p)
	if _, err := exec.JobRescueMany(ctx(t), &riverdriver.JobRescueManyParams{
		ID: []int64{id}, Error: [][]byte{errData}, FinalizedAt: []*time.Time{nil},
		ScheduledAt: []time.Time{now}, Schema: jobs.Schema, State: []string{string(rivertype.JobStateRetryable)},
		StuckHorizon: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("JobRescueMany: %v", err)
	}
	if _, err := exec.JobSchedule(ctx(t), &riverdriver.JobScheduleParams{Max: 100, Now: &now, Schema: jobs.Schema}); err != nil {
		t.Fatalf("JobSchedule: %v", err)
	}
}

// J1: the commit path. The domain row, the audit event and the completion
// are all visible, and the command-triggered event carries its causation and
// accountable principal (plan 8.2).
func TestJ1CommitPath(t *testing.T) {
	e := newJobsEnv(t)
	e.startRunner(t, runnerOpts{})
	id, eventID := e.enqueueCommand(t, "j1")
	e.waitState(t, id, "completed", 30*time.Second)
	if n := e.effects(t, id); n != 1 {
		t.Errorf("effects = %d, want 1", n)
	}
	var actor, trigger, causation, onBehalf string
	var attempt int
	if err := e.admin.QueryRow(ctx(t), `
		SELECT actor_principal_id::text, job_trigger, causation_event_id::text, on_behalf_of_principal_id::text, job_attempt
		FROM studio.audit_events WHERE action = $1 AND job_id = $2`, m6Applied, id).
		Scan(&actor, &trigger, &causation, &onBehalf, &attempt); err != nil {
		t.Fatalf("job event: %v", err)
	}
	if actor != workerID || trigger != "command" || causation != eventID || onBehalf != userU || attempt != 1 {
		t.Errorf("job event = actor %s trigger %s causation %s on_behalf %s attempt %d", actor, trigger, causation, onBehalf, attempt)
	}
}

// J2: a failure injected after JobCompleteTx but before commit leaves none of
// the three visible; the retry produces exactly one audit event.
func TestJ2FailureBeforeCommitRollsBackAll(t *testing.T) {
	testFailureBeforeCommit(t, func() error { return errors.New("injected failure before commit") })
}

// J3: the same with a panic.
func TestJ3PanicBeforeCommitRollsBackAll(t *testing.T) {
	testFailureBeforeCommit(t, func() error { panic("injected panic before commit") })
}

func testFailureBeforeCommit(t *testing.T, fail func() error) {
	e := newJobsEnv(t)
	var calls atomic.Int32
	firstSeen := newGate()
	e.startRunner(t, runnerOpts{hooks: jobs.Hooks{BeforeCommit: func(context.Context, pgx.Tx, jobs.JobRef) error {
		if calls.Add(1) == 1 {
			firstSeen.once.Do(func() { close(firstSeen.reached) })
			return fail()
		}
		return nil
	}}})
	id := e.enqueuePeriodic(t, effectArgs{Note: "j2"})
	firstSeen.awaitReached(t, "the injected failure")
	r := e.waitState(t, id, "completed", 30*time.Second)
	if r.attempt != 2 || r.errors != 1 {
		t.Errorf("job attempt %d with %d errors, want attempt 2 after 1 recorded failure", r.attempt, r.errors)
	}
	if n := e.effects(t, id); n != 1 {
		t.Errorf("effects = %d, want 1 (the failed attempt's write rolled back)", n)
	}
	if n := e.jobEvents(t, id); n != 1 {
		t.Errorf("job_execution events = %d, want exactly 1", n)
	}
	var attempt int
	if err := e.admin.QueryRow(ctx(t), `SELECT job_attempt FROM studio.audit_events WHERE job_id = $1`, id).Scan(&attempt); err != nil || attempt != 2 {
		t.Errorf("the surviving event is from attempt %d (%v), want 2", attempt, err)
	}
}

// J4a: the job is rescued and re-fetched while attempt 1 waits before the
// fence. The rescue uses River's own query (X3). Both orderings: attempt 2
// commits first, or attempt 1 is released first. Either way there is exactly
// one domain effect and the job eventually completes.
func TestJ4aRescuedAndRefetchedBeforeFence(t *testing.T) {
	for _, bFirst := range []bool{true, false} {
		name := map[bool]string{true: "attempt2_commits_first", false: "attempt1_released_first"}[bFirst]
		t.Run(name, func(t *testing.T) {
			e := newJobsEnv(t)
			gA, gB := newGate(), newGate()
			a := e.startRunner(t, runnerOpts{id: "client-a", hooks: jobs.Hooks{BeforeFence: pauseAttempt(1, gA)}})
			id := e.enqueuePeriodic(t, effectArgs{Note: name})
			gA.awaitReached(t, "attempt 1 before the fence")

			wp := e.pool(t, dbroles.Worker)
			rescue(t, wp, id)
			bHooks := jobs.Hooks{}
			if !bFirst {
				bHooks.BeforeFence = pauseAttempt(2, gB)
			}
			b := e.startRunner(t, runnerOpts{id: "client-b", hooks: bHooks})

			if bFirst {
				e.waitState(t, id, "completed", 30*time.Second)
				close(gA.release)
				stopRunner(t, a) // waits for attempt 1 to fail at the fence
			} else {
				gB.awaitReached(t, "attempt 2 before the fence")
				close(gA.release)
				stopRunner(t, a)
				close(gB.release)
				// Attempt 1's error made River mark the row retryable, so
				// attempt 2 is stale too; attempt 3 then runs and completes.
				waitFor(t, "the job to complete", 60*time.Second, func() bool {
					if e.job(t, id).state == "completed" {
						return true
					}
					now := time.Now().UTC()
					_, _ = riverExec(wp).JobSchedule(ctx(t), &riverdriver.JobScheduleParams{Max: 100, Now: &now, Schema: jobs.Schema})
					return false
				})
			}
			stopRunner(t, b)
			// The fence's own guarantee: a stale attempt never reaches the
			// handler. Without the attempt check, attempt 1 would run the
			// handler here (and only the post-completion check would roll
			// it back), so this assertion fails if the fence is removed.
			if n := e.handlerCalls(id); n != 1 {
				t.Errorf("handler invoked %d times across attempts, want exactly 1 (stale attempts must stop at the fence)", n)
			}
			if n := e.effects(t, id); n != 1 {
				t.Errorf("effects = %d, want exactly 1", n)
			}
			if n := e.jobEvents(t, id); n != 1 {
				t.Errorf("job_execution events = %d, want exactly 1", n)
			}
		})
	}
}

// J4b: a rescue attempted while the fence holds the row lock is blocked
// (checked with pg_blocking_pids); after the commit it updates nothing and
// the job stays completed.
func TestJ4bRescueBlockedByFence(t *testing.T) {
	e := newJobsEnv(t)
	g := newGate()
	var fencePID atomic.Int32
	e.startRunner(t, runnerOpts{hooks: jobs.Hooks{BeforeCommit: func(c context.Context, tx pgx.Tx, _ jobs.JobRef) error {
		var pid int32
		if err := tx.QueryRow(c, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return err
		}
		fencePID.Store(pid)
		g.wait(c)
		return nil
	}}})
	id := e.enqueuePeriodic(t, effectArgs{Note: "j4b"})
	g.awaitReached(t, "the fence holding the row lock")

	wp := e.pool(t, dbroles.Worker)
	done := make(chan error, 1)
	go func() {
		now := time.Now().UTC()
		errData, _ := json.Marshal(rivertype.AttemptError{At: now, Attempt: 1, Error: "rescue"})
		_, err := riverExec(wp).JobRescueMany(context.Background(), &riverdriver.JobRescueManyParams{
			ID: []int64{id}, Error: [][]byte{errData}, FinalizedAt: []*time.Time{nil},
			ScheduledAt: []time.Time{now}, Schema: jobs.Schema, State: []string{string(rivertype.JobStateRetryable)},
			StuckHorizon: now.Add(time.Hour),
		})
		done <- err
	}()
	waitFor(t, "the rescue to be blocked by the fence", 30*time.Second, func() bool {
		var n int
		if err := e.admin.QueryRow(ctx(t),
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY (pg_blocking_pids(pid))`, fencePID.Load()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	})
	close(g.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("rescue: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("rescue never returned")
	}
	r := e.job(t, id)
	if r.state != "completed" {
		t.Errorf("state = %s, want completed", r.state)
	}
	if _, rescued := r.metadata["river:rescue_count"]; rescued {
		t.Errorf("the rescue updated the completed job: %v", r.metadata)
	}
	if n := e.effects(t, id); n != 1 {
		t.Errorf("effects = %d, want 1", n)
	}
}

// rawAttempt1 is a River worker with no fence. On attempt 1 it waits at g and
// then returns nil, as a buggy handler would after a stale attempt.
type rawAttempt1 struct {
	river.WorkerDefaults[effectArgs]
	g *gate
}

func (w *rawAttempt1) Work(c context.Context, job *river.Job[effectArgs]) error {
	if job.Attempt == 1 {
		w.g.wait(c)
	}
	return nil
}

func startRawClient(t *testing.T, p *pgxpool.Pool, w river.Worker[effectArgs]) *river.Client[pgx.Tx] {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	c, err := river.NewClient(riverpgxv5.New(p), &river.Config{
		Schema: jobs.Schema, Logger: quietLogger(t), Workers: workers,
		Queues:            map[string]river.QueueConfig{jobs.DefaultQueue: {MaxWorkers: 1}},
		FetchPollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.WithoutCancel(ctx(t))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cc, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = c.StopAndCancel(cc)
	})
	return c
}

// J4c documents why the fence must never return nil on a stale attempt:
// attempt 1 returns nil after being rescued, River's completer (which has no
// attempt check, R2) marks the job completed under attempt 2, attempt 2 then
// finds it completed, and the job's work is never done.
func TestJ4cReturningNilOnStaleAttemptLosesTheJob(t *testing.T) {
	e := newJobsEnv(t)
	gA, gB := newGate(), newGate()
	a := startRawClient(t, e.pool(t, dbroles.Worker), &rawAttempt1{g: gA})
	id := e.enqueuePeriodic(t, effectArgs{Note: "j4c"})
	gA.awaitReached(t, "attempt 1 in the raw worker")
	rescue(t, e.pool(t, dbroles.Worker), id)
	b := e.startRunner(t, runnerOpts{id: "client-b", hooks: jobs.Hooks{BeforeFence: pauseAttempt(2, gB)}})
	gB.awaitReached(t, "attempt 2 before the fence")

	close(gA.release)
	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	close(gB.release)
	stopRunner(t, b)

	r := e.job(t, id)
	if r.state != "completed" || e.effects(t, id) != 0 || e.jobEvents(t, id) != 0 {
		t.Errorf("expected the lost job: completed with no effect and no event; got %s, %d effects, %d events",
			r.state, e.effects(t, id), e.jobEvents(t, id))
	}
}

// completeOutOfBand is a River worker with no fence: it moves its own job
// out of running behind River's back and then calls JobCompleteTx.
type completeOutOfBand struct {
	river.WorkerDefaults[effectArgs]
	admin  *pgxpool.Pool
	worker *pgxpool.Pool
	out    chan<- error
	state  chan<- rivertype.JobState
}

func (w *completeOutOfBand) Work(c context.Context, job *river.Job[effectArgs]) error {
	if _, err := w.admin.Exec(c, `UPDATE studio.river_job SET state = 'cancelled', finalized_at = now() WHERE id = $1`, job.ID); err != nil {
		w.out <- err
		return err
	}
	tx, err := w.worker.Begin(c)
	if err != nil {
		w.out <- err
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(c)) }()
	done, err := river.JobCompleteTx[*riverpgxv5.Driver](c, tx, job)
	w.out <- err
	if err == nil {
		w.state <- done.State
	}
	return nil
}

// J5a: library level, no fence. After an out-of-band state change,
// JobCompleteTx returns the unchanged row with no error (proves R3).
func TestJ5aCompleteTxReturnsUnchangedRowWithoutError(t *testing.T) {
	e := newJobsEnv(t)
	adminPool, err := pgxpool.New(ctx(t), e.db.URL(testdb.Admin))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	errs, states := make(chan error, 1), make(chan rivertype.JobState, 1)
	wp := e.pool(t, dbroles.Worker)
	startRawClient(t, wp, &completeOutOfBand{admin: adminPool, worker: wp, out: errs, state: states})
	e.enqueuePeriodic(t, effectArgs{Note: "j5a"})
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("JobCompleteTx returned an error: %v (R3 says it does not)", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("worker never ran")
	}
	if s := <-states; s == rivertype.JobStateCompleted {
		t.Errorf("JobCompleteTx reported %s for a job that was not running", s)
	}
}

// J5b: with the fence, the post-completion state check catches a completion
// that did not happen, exercised by skipping the fence's own check.
func TestJ5bPostCompletionStateCheck(t *testing.T) {
	e := newJobsEnv(t)
	logs := &logBuffer{}
	e.startRunner(t, runnerOpts{logs: logs, hooks: jobs.Hooks{
		SkipFenceCheck: true,
		BeforeFence: func(c context.Context, ref jobs.JobRef) {
			if _, err := e.admin.Exec(c, `UPDATE studio.river_job SET state = 'cancelled', finalized_at = now() WHERE id = $1`, ref.ID); err != nil {
				t.Error(err)
			}
		},
	}})
	id := e.enqueuePeriodic(t, effectArgs{Note: "j5b"})
	waitFor(t, "the fence to report the failed completion", 30*time.Second, func() bool {
		return strings.Contains(logs.String(), "completion did not leave the job completed")
	})
	if n := e.effects(t, id); n != 0 {
		t.Errorf("effects = %d, want 0 (rolled back)", n)
	}
	if n := e.jobEvents(t, id); n != 0 {
		t.Errorf("job_execution events = %d, want 0 (rolled back)", n)
	}
}

// J5c: a cancellation requested while the attempt waits is detected at the
// fence (cancel_attempted_at, set exactly as River's JobCancel sets it, but
// without the notification that would cancel the context first).
func TestJ5cCancellationDetectedAtFence(t *testing.T) {
	e := newJobsEnv(t)
	g := newGate()
	logs := &logBuffer{}
	e.startRunner(t, runnerOpts{logs: logs, hooks: jobs.Hooks{BeforeFence: pauseAttempt(1, g)}})
	id := e.enqueuePeriodic(t, effectArgs{Note: "j5c"})
	g.awaitReached(t, "attempt 1 before the fence")
	if _, err := e.admin.Exec(ctx(t), `
		UPDATE studio.river_job SET metadata = metadata || jsonb_build_object('cancel_attempted_at', now()) WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	e.waitState(t, id, "cancelled", 30*time.Second)
	if !strings.Contains(logs.String(), "cancellation was requested") {
		t.Errorf("the fence did not report the cancellation; logs:\n%s", logs.String())
	}
	if n := e.effects(t, id); n != 0 {
		t.Errorf("effects = %d, want 0", n)
	}
}

// J6: a job enqueued in a rolled-back transaction never exists or runs; one
// enqueued in a committed transaction is invisible until commit and runs
// once.
func TestJ6EnqueueFollowsTheTransaction(t *testing.T) {
	e := newJobsEnv(t)
	var rolledBack int64
	err := db.WithTx(ctx(t), e.app, func(tx pgx.Tx) error {
		id, err := e.queue.InsertTx(ctx(t), tx, effectArgs{Note: "rolled back"}, jobs.Origin{})
		rolledBack = id
		if err != nil {
			return err
		}
		return errors.New("roll back")
	})
	if err == nil || rolledBack == 0 {
		t.Fatalf("setup: %v, id %d", err, rolledBack)
	}

	tx, err := e.app.Begin(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	committed, err := e.queue.InsertTx(ctx(t), tx, effectArgs{Note: "committed"}, jobs.Origin{})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, e.admin, `SELECT count(*) FROM studio.river_job WHERE id = $1`, committed); n != 0 {
		t.Errorf("job visible before commit")
	}
	if err := tx.Commit(ctx(t)); err != nil {
		t.Fatal(err)
	}

	e.startRunner(t, runnerOpts{})
	e.waitState(t, committed, "completed", 30*time.Second)
	if n := countRows(t, e.admin, `SELECT count(*) FROM studio.river_job WHERE id = $1`, rolledBack); n != 0 {
		t.Errorf("rolled-back job exists")
	}
	if n := e.effects(t, rolledBack); n != 0 {
		t.Errorf("rolled-back job ran")
	}
	if n := e.effects(t, committed); n != 1 {
		t.Errorf("committed job ran %d times, want 1", n)
	}
}

func countRows(t *testing.T, conn *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx(t), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type badKindArgs struct{}

// Kind is longer than river_job's kind_length CHECK allows, so the insert
// fails inside PostgreSQL.
func (badKindArgs) Kind() string { return strings.Repeat("k", 200) }

// J7: a River error inside a domain transaction rolls the whole transaction
// back (R4).
func TestJ7RiverErrorRollsBackTheDomainTransaction(t *testing.T) {
	e := newJobsEnv(t)
	err := db.WithTx(ctx(t), e.app, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx(t), `INSERT INTO studio.m6_effects (job_id, attempt, note) VALUES (0, 1, 'j7')`); err != nil {
			return err
		}
		_, err := e.queue.InsertTx(ctx(t), tx, badKindArgs{}, jobs.Origin{})
		return err
	})
	if err == nil {
		t.Fatal("River insert of an invalid job succeeded")
	}
	wantSQLState(t, err, checkViolation, "kind_length", "River insert error")
	if n := countRows(t, e.admin, `SELECT count(*) FROM studio.m6_effects WHERE note = 'j7'`); n != 0 {
		t.Errorf("domain write survived the River error")
	}
	if n := countRows(t, e.admin, `SELECT count(*) FROM studio.river_job`); n != 0 {
		t.Errorf("%d jobs exist after the rollback", n)
	}
}

// J8 / G5: both roles run their River operations with only their generated
// grants, and are refused what the matrix withholds (plan 6.3).
func TestJ8RolesRunRiverWithTheirGrants(t *testing.T) {
	e := newJobsEnv(t)
	// API role: insert (R5) and the job list read.
	id := e.enqueuePeriodic(t, effectArgs{Note: "j8"})
	appConn := e.db.Connect(t, dbroles.App)
	if n := countRows(t, appConn, `SELECT count(*) FROM studio.river_job WHERE id = $1`, id); n != 1 {
		t.Errorf("api role cannot read its job")
	}
	for _, stmt := range []string{
		`DELETE FROM studio.river_job`,
		`UPDATE studio.river_job SET state = 'completed'`,
		`TRUNCATE studio.river_job`,
		`REINDEX TABLE studio.river_job`,
		`SELECT * FROM studio.river_leader`,
		`SELECT * FROM studio.river_queue`,
	} {
		wantSQLState(t, exec(t, appConn, stmt), insufficientPriv, "", "api: "+stmt)
	}

	// Worker role: fetch, fence and complete; leader election and queue
	// bookkeeping; the cleaner's delete; the reindexer's MAINTAIN.
	e.startRunner(t, runnerOpts{})
	e.waitState(t, id, "completed", 30*time.Second)
	waitFor(t, "a leader to be elected", 30*time.Second, func() bool {
		return countRows(t, e.admin, `SELECT count(*) FROM studio.river_leader`) == 1
	})
	if n := countRows(t, e.admin, `SELECT count(*) FROM studio.river_queue WHERE name = $1`, jobs.DefaultQueue); n != 1 {
		t.Errorf("worker did not record its queue")
	}
	wp := e.pool(t, dbroles.Worker)
	if _, err := riverExec(wp).JobDeleteBefore(ctx(t), &riverdriver.JobDeleteBeforeParams{
		CompletedDoDelete: true, CompletedFinalizedAtHorizon: time.Now().Add(time.Hour),
		Max: 100, Schema: jobs.Schema,
	}); err != nil {
		t.Errorf("worker cleaner delete: %v", err)
	}
	workerConn := e.db.Connect(t, dbroles.Worker)
	// River's reindexer path: REINDEX INDEX CONCURRENTLY through its driver
	// (MAINTAIN, R7), outside any transaction.
	if err := riverExec(wp).IndexReindex(ctx(t), &riverdriver.IndexReindexParams{Index: "river_job_kind", Schema: jobs.Schema}); err != nil {
		t.Errorf("worker reindex via River (MAINTAIN): %v", err)
	}
	wantOK(t, exec(t, workerConn, `SELECT count(*) FROM studio.river_migration`), "worker reads river_migration")
	wantOK(t, exec(t, workerConn, `INSERT INTO studio.river_notification (payload, topic) VALUES ('{}', 't')`), "worker notification outbox")
	for _, stmt := range []string{
		`TRUNCATE studio.river_job`,
		`UPDATE studio.river_migration SET version = 9`,
		`DROP INDEX studio.river_job_kind`,
		`ALTER TABLE studio.river_job DISABLE TRIGGER ALL`,
	} {
		wantSQLState(t, exec(t, workerConn, stmt), insufficientPriv, "", "worker: "+stmt)
	}
	for _, role := range []string{dbroles.Backup, dbroles.ReadOnly} {
		conn := e.db.Connect(t, role)
		wantSQLState(t, exec(t, conn, `DELETE FROM studio.river_job`), insufficientPriv, "", role+": delete river_job")
	}
}

// J10: the unique index rejects a second job_execution event for a job.
func TestJ10UniqueJobExecutionEvent(t *testing.T) {
	e := newJobsEnv(t)
	e.startRunner(t, runnerOpts{})
	id := e.enqueuePeriodic(t, effectArgs{Note: "j10"})
	e.waitState(t, id, "completed", 30*time.Second)
	w := audit.NewWriter(m6Catalog)
	workerConn := e.db.Connect(t, dbroles.Worker)
	_, err := w.Record(ctx(t), workerConn, audit.Event{
		Action: m6Applied, Outcome: audit.Succeeded, ActorPrincipalID: workerID, ActorKind: audit.System,
		Job: &audit.Job{ID: id, Attempt: 2, Trigger: audit.TriggerPeriodic},
	})
	wantSQLState(t, err, uniqueViolation, "audit_events_job_execution_job_id_key", "second job_execution event")
}

// J9 helper process: run the worker and hold the fenced transaction open
// until killed. It does nothing unless started by TestJ9.
func TestJ9HelperWorkerProcess(t *testing.T) {
	url := os.Getenv("STUDIO_M6_HELPER_WORKER_URL")
	if url == "" {
		return // not the helper process
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		fmt.Println("M6-HELPER-ERROR", err)
		os.Exit(1)
	}
	reg := jobs.NewRegistry()
	if err := jobs.Register(reg, applyEffect, jobs.KindOptions{Action: m6Applied}); err != nil {
		fmt.Println("M6-HELPER-ERROR", err)
		os.Exit(1)
	}
	r, err := jobs.NewRunner(jobs.RunnerConfig{
		Pool: pool, Registry: reg, Catalog: m6Catalog, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Concurrency: 1, FetchPollInterval: 100 * time.Millisecond,
		Hooks: jobs.Hooks{BeforeCommit: func(context.Context, pgx.Tx, jobs.JobRef) error {
			fmt.Println("M6-HELPER-READY")
			select {} // hold the transaction until SIGKILL
		}},
	})
	if err == nil {
		err = r.Start(context.Background())
	}
	if err != nil {
		fmt.Println("M6-HELPER-ERROR", err)
		os.Exit(1)
	}
	select {}
}

// J9: killing the worker before commit leads River's rescuer to re-run the
// job, with exactly one audit event.
func TestJ9KilledWorkerIsRescued(t *testing.T) {
	e := newJobsEnv(t)
	id := e.enqueuePeriodic(t, effectArgs{Note: "j9"})

	self, err := os.Executable() // this test binary, re-run as the helper
	if err != nil {
		t.Fatal(err)
	}
	cmd := osexec.CommandContext(ctx(t), self, "-test.run=^TestJ9HelperWorkerProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "STUDIO_M6_HELPER_WORKER_URL="+e.db.URL(dbroles.Worker))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			switch line := sc.Text(); {
			case line == "M6-HELPER-READY":
				ready <- nil
				return
			case strings.HasPrefix(line, "M6-HELPER-ERROR"):
				ready <- errors.New(line)
				return
			}
		}
		ready <- errors.New("helper exited before reaching the commit")
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("helper never reached the commit")
	}
	if err := cmd.Process.Kill(); err != nil { // SIGKILL: no cleanup runs
		t.Fatal(err)
	}
	_ = cmd.Wait()

	if r := e.job(t, id); r.state != "running" || r.attempt != 1 {
		t.Fatalf("after the kill the job is %s at attempt %d, want running at attempt 1", r.state, r.attempt)
	}
	if n := e.effects(t, id); n != 0 {
		t.Fatalf("the killed attempt's write is visible")
	}

	// A fresh worker with short rescue and timeout settings. The rescuer runs
	// as soon as this client is elected leader (after the dead client's
	// leadership expires).
	e.startRunner(t, runnerOpts{rescue: time.Second, timeout: time.Second})
	r := e.waitState(t, id, "completed", 120*time.Second)
	if r.attempt < 2 {
		t.Errorf("completed at attempt %d, want a rescued re-run", r.attempt)
	}
	if _, rescued := r.metadata["river:rescue_count"]; !rescued {
		t.Errorf("the job was not rescued by River: %v", r.metadata)
	}
	if n := e.effects(t, id); n != 1 {
		t.Errorf("effects = %d, want 1", n)
	}
	if n := e.jobEvents(t, id); n != 1 {
		t.Errorf("job_execution events = %d, want 1", n)
	}
}

// R8: migration 00005 is River v0.49.0's schema. River's own migrator is run
// into a scratch schema and the two catalogs must be identical apart from the
// schema name.
func TestRiverSchemaMatchesRiverMigrations(t *testing.T) {
	d := testdb.New(t)
	adminPool, err := pgxpool.New(ctx(t), d.URL(testdb.Admin))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	if _, err := adminPool.Exec(ctx(t), `CREATE SCHEMA river_ref`); err != nil {
		t.Fatal(err)
	}
	m, err := rivermigrate.New(riverpgxv5.New(adminPool), &rivermigrate.Config{Schema: "river_ref", Logger: quietLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Migrate(ctx(t), rivermigrate.DirectionUp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Versions); n != 8 {
		t.Fatalf("River applied %d migrations; the pinned main line has 8", n)
	}
	admin := d.Connect(t, testdb.Admin)
	strip := regexp.MustCompile(`\b(studio|river_ref)\.`)
	describe := func(schema string) []string {
		var out []string
		for _, q := range []string{
			// Columns of River's tables, with types, nullability and defaults.
			`SELECT c.relname || '.' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod) || ' notnull=' || a.attnotnull
			        || ' default=' || coalesce(pg_get_expr(ad.adbin, ad.adrelid), '') || ' #' || a.attnum
			 FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
			 JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = $1
			 LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
			 WHERE c.relname LIKE 'river\_%' AND c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped`,
			// Tables and sequences with persistence (river_leader is unlogged).
			`SELECT c.relname || ' kind=' || c.relkind::text || ' persistence=' || c.relpersistence::text
			 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = $1
			 WHERE c.relname LIKE 'river\_%' AND c.relkind IN ('r', 'S')`,
			`SELECT co.conrelid::regclass::text || ' ' || co.conname || ' ' || pg_get_constraintdef(co.oid)
			 FROM pg_constraint co JOIN pg_class c ON c.oid = co.conrelid
			 WHERE co.connamespace = $1::regnamespace AND c.relname LIKE 'river\_%'`,
			`SELECT indexname || ' ' || indexdef FROM pg_indexes WHERE schemaname = $1 AND tablename LIKE 'river\_%'`,
			`SELECT t.typname || ' ' || string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
			 FROM pg_type t JOIN pg_enum e ON e.enumtypid = t.oid
			 WHERE t.typnamespace = $1::regnamespace AND t.typname LIKE 'river\_%' GROUP BY t.typname`,
			// Function bodies compared with whitespace collapsed.
			`SELECT p.proname || ' ' || pg_get_function_identity_arguments(p.oid) || ' '
			        || md5(btrim(regexp_replace(p.prosrc, '\s+', ' ', 'g'))) || ' ' || p.provolatile::text || ' ' || (SELECT lanname FROM pg_language WHERE oid = p.prolang)
			 FROM pg_proc p WHERE p.pronamespace = $1::regnamespace AND p.proname LIKE 'river\_%'`,
			`SELECT tgrelid::regclass::text || ' ' || tgname FROM pg_trigger
			 WHERE NOT tgisinternal AND tgrelid IN (SELECT oid FROM pg_class WHERE relnamespace = $1::regnamespace AND relname LIKE 'river\_%')`,
		} {
			for _, row := range collect(t, admin, q, schema) {
				out = append(out, strip.ReplaceAllString(row, ""))
			}
		}
		return out
	}
	diff(t, "River schema (studio vs River's migrator)", describe("studio"), describe("river_ref"))
	if n := countRows(t, admin, `SELECT count(*) FROM studio.river_migration WHERE line = 'main' AND version BETWEEN 1 AND 8`); n != 8 {
		t.Errorf("river_migration records %d of River's 8 main-line versions", n)
	}
}

// The idempotency harness (plan 12.1, 17.3 #8) detects a non-idempotent
// kind and accepts an idempotent one. Each run enqueues the kind's fixture
// and waits for completion; the digest covers only domain state.
func TestIdempotencyHarness(t *testing.T) {
	e := newJobsEnv(t)
	e.startRunner(t, runnerOpts{register: func(r *jobs.Registry) error {
		if err := jobs.Register(r, upsertNote, jobs.KindOptions{Action: m6Applied}); err != nil {
			return err
		}
		return jobs.Register(r, applyEffect, jobs.KindOptions{Action: m6Applied})
	}})
	runOnce := func(args jobs.Args) func(context.Context) error {
		return func(context.Context) error {
			id := e.enqueuePeriodic(t, args)
			e.waitState(t, id, "completed", 30*time.Second)
			return nil
		}
	}
	digest := func(sql string) func(context.Context) (string, error) {
		return func(c context.Context) (string, error) {
			var s string
			err := e.admin.QueryRow(c, sql).Scan(&s)
			return s, err
		}
	}
	if err := jobstest.CheckIdempotent(ctx(t), runOnce(noteArgs{Note: "n1"}),
		digest(`SELECT coalesce(string_agg(note, ',' ORDER BY note), '') FROM studio.m6_notes`)); err != nil {
		t.Errorf("idempotent kind rejected: %v", err)
	}
	if err := jobstest.CheckIdempotent(ctx(t), runOnce(effectArgs{Note: "e1"}),
		digest(`SELECT count(*)::text FROM studio.m6_effects WHERE note = 'e1'`)); err == nil {
		t.Error("non-idempotent kind accepted")
	}
}

type noteArgs struct {
	Note string `json:"note"`
}

func (noteArgs) Kind() string { return "m6.note" }

func upsertNote(c context.Context, tx pgx.Tx, job jobs.Job[noteArgs]) (jobs.Result, error) {
	_, err := tx.Exec(c, `INSERT INTO studio.m6_notes (note) VALUES ($1) ON CONFLICT DO NOTHING`, job.Args.Note)
	return jobs.Result{TargetType: "m6_note", TargetID: job.Args.Note}, err
}

// Plan 17.3 #8 and 8.1: every production job kind writes a cataloged
// job_execution action and has an idempotency test. Each kind added later
// must add its test here.
func TestEveryProductionJobKindIsCoveredByTheHarness(t *testing.T) {
	store, err := fsstore.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	maint, err := blob.NewMaintenance(blob.MaintenanceConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := jobkinds.Production(jobkinds.Deps{Blob: maint})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.CheckActions(audit.Actions); err != nil {
		t.Error(err)
	}
	// kind -> the test that runs it twice through jobstest.CheckIdempotent
	fixtures := map[string]string{
		blob.KindSweep: "TestBlobSweepIsIdempotent",
		blob.KindAudit: "TestBlobAuditIsIdempotent",
	}
	for _, kind := range reg.Kinds() {
		if fixtures[kind] == "" {
			t.Errorf("job kind %s has no idempotency test", kind)
		}
	}
}
