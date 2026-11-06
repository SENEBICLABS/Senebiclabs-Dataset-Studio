package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals"
)

// Hooks inject faults into the fence for tests (J2-J5). Production code
// leaves them zero. They are passed in through RunnerConfig, never held in
// package state.
type Hooks struct {
	// BeforeFence runs after River hands over the attempt and before the
	// fenced transaction begins.
	BeforeFence func(ctx context.Context, job JobRef)
	// SkipFenceCheck disables the row lock and attempt check, so tests can
	// prove the post-completion state check on its own (J5b).
	SkipFenceCheck bool
	// BeforeCommit runs after JobCompleteTx, with the row lock still held.
	// A returned error, or a panic, aborts the transaction (J2, J3).
	BeforeCommit func(ctx context.Context, tx pgx.Tx, job JobRef) error
}

type fence struct {
	pool   *pgxpool.Pool
	writer *audit.Writer
	hooks  Hooks
}

// fenceSQL locks the job row and reads what the fence checks (plan 12.1).
// The lock makes River's rescuer wait for this transaction and then match
// nothing, because the row is no longer running (plan 3.3, test J4b).
const fenceSQL = `
SELECT state::text, attempt, metadata ? 'cancel_attempted_at'
FROM studio.river_job
WHERE id = $1
FOR UPDATE`

// check enforces the fence: the job is still running this attempt and no
// cancellation was requested. Any other outcome is an error.
func check(ctx context.Context, tx pgx.Tx, ref JobRef) error {
	var (
		state     string
		attempt   int
		cancelled bool
	)
	err := tx.QueryRow(ctx, fenceSQL, ref.ID).Scan(&state, &attempt, &cancelled)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w (job %d no longer exists)", ErrStaleAttempt, ref.ID)
	case err != nil:
		return fmt.Errorf("jobs: fence job %d: %w", ref.ID, err)
	case cancelled:
		return fmt.Errorf("%w (job %d attempt %d)", ErrCancelled, ref.ID, ref.Attempt)
	case state != string(rivertype.JobStateRunning) || attempt != ref.Attempt:
		return fmt.Errorf("%w (job %d: attempt %d, row is %s at attempt %d)", ErrStaleAttempt, ref.ID, ref.Attempt, state, attempt)
	}
	return nil
}

// run executes one attempt inside the fenced transaction of plan 12.1:
//
//	BEGIN
//	  lock the job row; require running, this attempt, no cancellation
//	  handler's domain writes
//	  one job_execution audit event
//	  JobCompleteTx; require the returned state to be completed (R3)
//	COMMIT
//
// Any failure rolls back everything, so the domain change, the audit event
// and the completion are visible together or not at all.
func run[A Args](ctx context.Context, f *fence, rj *river.Job[A], h HandlerFunc[A], action string) error {
	origin := originFromMetadata(rj.Metadata)
	job := Job[A]{JobRef: JobRef{ID: rj.ID, Attempt: rj.Attempt}, Args: rj.Args, Origin: origin}
	if f.hooks.BeforeFence != nil {
		f.hooks.BeforeFence(ctx, job.JobRef)
	}
	if err := origin.Validate(); err != nil {
		return fmt.Errorf("job %d: %w", rj.ID, err)
	}
	return db.WithTx(ctx, f.pool, func(tx pgx.Tx) error {
		if !f.hooks.SkipFenceCheck {
			if err := check(ctx, tx, job.JobRef); err != nil {
				return err
			}
		}
		res, err := h(ctx, tx, job)
		if err != nil {
			return fmt.Errorf("job %d (%s) attempt %d: %w", rj.ID, rj.Kind, rj.Attempt, err)
		}
		if _, err := f.writer.Record(ctx, tx, audit.Event{
			Action:           action,
			Outcome:          audit.Succeeded,
			ActorPrincipalID: principals.WorkerID,
			ActorKind:        audit.System,
			OnBehalfOf:       origin.OnBehalfOf,
			TargetType:       res.TargetType,
			TargetID:         res.TargetID,
			ProjectID:        res.ProjectID,
			DatasetID:        res.DatasetID,
			Job:              &audit.Job{ID: rj.ID, Attempt: rj.Attempt, Trigger: origin.Trigger()},
			CausationEventID: origin.CausationEventID,
			Details:          res.Details,
		}); err != nil {
			return err
		}
		done, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, rj)
		if err != nil {
			// R4: River no longer uses savepoints; WithTx rolls back.
			return fmt.Errorf("jobs: complete job %d: %w", rj.ID, err)
		}
		if done.State != rivertype.JobStateCompleted {
			// R3: a job that was not running comes back unchanged, with no
			// error.
			return fmt.Errorf("%w (job %d is %s)", ErrNotCompleted, rj.ID, done.State)
		}
		if f.hooks.BeforeCommit != nil {
			if err := f.hooks.BeforeCommit(ctx, tx, job.JobRef); err != nil {
				return err
			}
		}
		return nil
	})
}
