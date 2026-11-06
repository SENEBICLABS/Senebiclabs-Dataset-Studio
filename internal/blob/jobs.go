package blob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob/blobdb"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/jobs"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals"
)

// Job kinds (plan 6.3, 11).
const (
	KindSweep = "blob.sweep"
	KindAudit = "blob.audit"
)

// Schedule and grace periods. The sweep interval and the 24-hour grace are
// the plan's (11; approved default "24-hour blob grace periods"). The plan
// sets no blob.audit interval: daily is an Assumption (plan 11), a default
// Godwin accepted on 2026-11-07, not a plan requirement.
const (
	SweepEvery   = 15 * time.Minute
	AuditEvery   = 24 * time.Hour
	DefaultGrace = 24 * time.Hour
)

// SweepArgs are blob.sweep's arguments (none: it is periodic).
type SweepArgs struct{}

// Kind implements jobs.Args.
func (SweepArgs) Kind() string { return KindSweep }

// AuditArgs are blob.audit's arguments (none: it is periodic).
type AuditArgs struct{}

// Kind implements jobs.Args.
func (AuditArgs) Kind() string { return KindAudit }

// SweepDetails is the details struct of blob.pending_swept, version 1: one
// event per sweep (plan 8.1).
type SweepDetails struct {
	Skipped          bool   `json:"skipped"`
	SkippedReason    string `json:"skipped_reason,omitempty"`
	UploadsAbandoned int    `json:"uploads_abandoned"`
	StagingRemoved   int    `json:"staging_files_removed"`
	FinalsRemoved    int    `json:"final_files_removed"`
}

// AuditDetails is the details struct of blob.audited, version 1.
type AuditDetails struct {
	Checked        int `json:"checked"`
	Missing        int `json:"missing"`
	Corrupt        int `json:"corrupt"`
	EventsRecorded int `json:"health_events_recorded"`
}

// Maintenance holds the dependencies of blob.sweep and blob.audit.
type Maintenance struct {
	store Store
	grace time.Duration
	now   func() time.Time
}

// MaintenanceConfig configures Maintenance. Zero values take the plan's
// defaults; tests inject the clock and grace instead of waiting 24 hours.
type MaintenanceConfig struct {
	Store Store
	Grace time.Duration
	Now   func() time.Time
}

// NewMaintenance returns the blob job handlers.
func NewMaintenance(cfg MaintenanceConfig) (*Maintenance, error) {
	if cfg.Store == nil {
		return nil, errors.New("blob: maintenance needs a store")
	}
	m := &Maintenance{store: cfg.Store, grace: cfg.Grace, now: cfg.Now}
	if m.grace <= 0 {
		m.grace = DefaultGrace
	}
	if m.now == nil {
		m.now = time.Now
	}
	return m, nil
}

// Register adds blob.sweep (every 15 minutes) and blob.audit (daily).
func Register(r *jobs.Registry, m *Maintenance) error {
	if err := jobs.Register(r, m.Sweep, jobs.KindOptions{Action: audit.ActionBlobPendingSwept, Every: SweepEvery}); err != nil {
		return err
	}
	return jobs.Register(r, m.Audit, jobs.KindOptions{Action: audit.ActionBlobAudited, Every: AuditEvery})
}

// Sweep is blob.sweep (plan 11, recovery table). It runs only while it holds
// BACKUP_GC exclusively, so it never deletes anything a backup in progress
// may be copying. Every step is idempotent: a file already gone is not an
// error, and a row already abandoned is skipped.
func (m *Maintenance) Sweep(ctx context.Context, tx pgx.Tx, _ jobs.Job[SweepArgs]) (jobs.Result, error) {
	q := blobdb.New(tx)
	res := jobs.Result{TargetType: "blob_store"}
	ok, err := q.TryLockBackupGC(ctx, blobdb.TryLockBackupGCParams{Namespace: LockNamespaceBackupGC, Key: 0})
	if err != nil {
		return res, fmt.Errorf("blob.sweep: BACKUP_GC: %w", err)
	}
	if !ok {
		res.Details = SweepDetails{Skipped: true, SkippedReason: "BACKUP_GC held"}
		return res, nil
	}
	now := m.now()
	var d SweepDetails

	// Crash during streaming: after expires_at, delete the file and mark the
	// upload abandoned.
	expired, err := q.ListExpiredUploads(ctx, now)
	if err != nil {
		return res, fmt.Errorf("blob.sweep: expired uploads: %w", err)
	}
	for _, u := range expired {
		if err := m.store.RemoveStaging(u.StagingKey); err != nil {
			return res, err
		}
		n, err := q.AbandonUpload(ctx, u.ID)
		if err != nil {
			return res, fmt.Errorf("blob.sweep: abandon %s: %w", u.ID, err)
		}
		d.UploadsAbandoned += int(n)
	}

	// Crash before transaction 1 committed, and leftovers of finished
	// uploads: a staging file older than the grace period whose upload is
	// unknown or no longer staging.
	staging, err := m.store.Staging()
	if err != nil {
		return res, err
	}
	for _, e := range staging {
		if now.Sub(e.ModTime) < m.grace {
			continue
		}
		up, err := q.GetUploadByStagingKey(ctx, e.Key)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return res, fmt.Errorf("blob.sweep: look up staging file: %w", err)
		case up.State == "staging":
			continue // still in flight, or expired and handled above
		}
		if err := m.store.RemoveStaging(e.Key); err != nil {
			return res, err
		}
		d.StagingRemoved++
	}

	// Crash after the rename and before transaction 2 committed: a final file
	// with no row, older than the grace period. It is deleted only after
	// taking the per-sha lock and re-checking, so an upload committing that
	// content right now is waited for and its file kept.
	finals, err := m.store.Finals()
	if err != nil {
		return res, err
	}
	for _, e := range finals {
		if now.Sub(e.ModTime) < m.grace {
			continue
		}
		if err := q.LockBlobKey(ctx, blobdb.LockBlobKeyParams{Namespace: LockNamespaceBlob, Key: BlobKey(e.Sum)}); err != nil {
			return res, fmt.Errorf("blob.sweep: lock: %w", err)
		}
		exists, err := q.BlobExists(ctx, e.Sum[:])
		if err != nil {
			return res, fmt.Errorf("blob.sweep: check %s: %w", e.Sum, err)
		}
		if exists {
			continue
		}
		if err := m.store.RemoveFinal(e.Sum); err != nil {
			return res, err
		}
		d.FinalsRemoved++
	}
	res.Details = d
	return res, nil
}

// Audit is blob.audit: it re-hashes every committed blob and records each
// missing or corrupt one in blob_health_events (plan 11). It never repairs
// anything; the fix is restoring from backup. Re-detection is a no-op, so
// the job is idempotent.
func (m *Maintenance) Audit(ctx context.Context, tx pgx.Tx, _ jobs.Job[AuditArgs]) (jobs.Result, error) {
	q := blobdb.New(tx)
	blobs, err := q.ListBlobs(ctx)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("blob.audit: list: %w", err)
	}
	worker := uuid.MustParse(principals.WorkerID)
	var d AuditDetails
	for _, b := range blobs {
		sum, err := SumFromBytes(b.Sha256)
		if err != nil {
			return jobs.Result{}, err
		}
		h, err := m.store.Verify(sum)
		if err != nil {
			return jobs.Result{}, err
		}
		d.Checked++
		if h == Healthy {
			continue
		}
		if h == Missing {
			d.Missing++
		} else {
			d.Corrupt++
		}
		details, _ := json.Marshal(map[string]string{"sha256": sum.String()})
		n, err := q.InsertHealthEvent(ctx, blobdb.InsertHealthEventParams{
			BlobObjectID: b.ID, Kind: string(h), Details: details, CreatedBy: worker,
		})
		if err != nil {
			return jobs.Result{}, fmt.Errorf("blob.audit: record %s: %w", sum, err)
		}
		d.EventsRecorded += int(n)
	}
	return jobs.Result{TargetType: "blob_store", Details: d}, nil
}

// HealthChecker is the /readyz check of plan 11: readiness fails while any
// blob health event exists, because a missing or corrupt committed blob is
// not self-healing.
type HealthChecker struct {
	Pool *pgxpool.Pool
}

// Name implements health.Checker.
func (HealthChecker) Name() string { return "blob_store" }

// Check implements health.Checker.
func (c HealthChecker) Check(ctx context.Context) error {
	n, err := blobdb.New(c.Pool).CountHealthEvents(ctx)
	if err != nil {
		return fmt.Errorf("blob health: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("blob health: %d missing or corrupt blob event(s); restore from backup", n)
	}
	return nil
}
