//go:build integration

package integration

// M5 blob store (plan 5.3, 11, 16.2): the upload protocol, B1-B7, database
// enforcement of the ledger's rules, the grants, and the idempotency of
// blob.sweep and blob.audit. B8 (C1 re-run on this protocol) belongs to M7.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob/fsstore"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/jobs"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/jobs/jobstest"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db"
)

type blobEnv struct {
	*jobsEnv
	root   string
	store  *fsstore.Store
	svc    *blob.Service
	offset atomic.Int64 // added to the wall clock the sweep sees
	user   uuid.UUID
}

const testMaxUpload = 1 << 20

func newBlobEnv(t *testing.T) *blobEnv {
	t.Helper()
	e := &blobEnv{jobsEnv: newJobsEnv(t), root: filepath.Join(t.TempDir(), "blobs"), user: uuid.MustParse(userU)}
	s, err := fsstore.New(e.root)
	if err != nil {
		t.Fatal(err)
	}
	e.store = s
	if e.svc, err = blob.NewService(s, testMaxUpload); err != nil {
		t.Fatal(err)
	}
	maint, err := blob.NewMaintenance(blob.MaintenanceConfig{
		Store: s,
		Now:   func() time.Time { return time.Now().Add(time.Duration(e.offset.Load())) },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.startRunner(t, runnerOpts{catalog: audit.Actions, register: func(r *jobs.Registry) error { return blob.Register(r, maint) }})
	return e
}

// later moves the sweep's clock past the 24-hour grace and upload expiry.
func (e *blobEnv) later() { e.offset.Store(int64(25 * time.Hour)) }

// upload runs both transactions as the API role and commits.
func (e *blobEnv) upload(t *testing.T, data []byte) blob.Object {
	t.Helper()
	st, err := e.svc.Stage(ctx(t), e.app, bytes.NewReader(data), e.user)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	var obj blob.Object
	if err := db.WithTx(ctx(t), e.app, func(tx pgx.Tx) error {
		var err error
		obj, err = e.svc.Commit(ctx(t), tx, st, e.user)
		return err
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return obj
}

func (e *blobEnv) runJob(t *testing.T, args jobs.Args) map[string]any {
	t.Helper()
	id := e.enqueuePeriodic(t, args)
	waitFor(t, fmt.Sprintf("job %d (%s) to complete", id, args.Kind()), 30*time.Second, func() bool {
		r := e.job(t, id)
		if r.errors > 0 {
			var last string
			_ = e.admin.QueryRow(ctx(t), `SELECT errors[array_length(errors, 1)]->>'error' FROM studio.river_job WHERE id = $1`, id).Scan(&last)
			t.Fatalf("job %d (%s) failed: %s", id, args.Kind(), last)
		}
		return r.state == "completed"
	})
	var raw []byte
	if err := e.admin.QueryRow(ctx(t), `SELECT details FROM studio.audit_events WHERE job_id = $1 AND action_class = 'job_execution'`, id).Scan(&raw); err != nil {
		t.Fatalf("job %d event: %v", id, err)
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *blobEnv) sweep(t *testing.T) map[string]any { return e.runJob(t, blob.SweepArgs{}) }

func (e *blobEnv) uploadState(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.admin.QueryRow(ctx(t), `SELECT state FROM studio.blob_uploads WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *blobEnv) finalExists(sum blob.Sum) bool {
	h := sum.String()
	_, err := os.Stat(filepath.Join(e.root, "sha256", h[:2], h[2:4], h))
	return err == nil
}

func (e *blobEnv) stagingFiles(t *testing.T) []string {
	t.Helper()
	st, err := e.store.Staging()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, s := range st {
		keys = append(keys, s.Key)
	}
	return keys
}

// writeStaging creates a staging file directly, as a crashed upload leaves it.
func (e *blobEnv) writeStaging(t *testing.T, key string, data []byte) {
	t.Helper()
	w, err := e.store.CreateStaging(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func num(d map[string]any, k string) int {
	f, _ := d[k].(float64)
	return int(f)
}

// The commit path: both transactions as the API role, the ledger row, the
// promoted upload, the sniffed media type and the durable final file.
func TestBlobUploadCommit(t *testing.T) {
	e := newBlobEnv(t)
	data := []byte("plain text content for the blob store")
	obj := e.upload(t, data)
	if obj.Sum != sha256.Sum256(data) || obj.Size != int64(len(data)) || obj.Deduplicated {
		t.Errorf("object = %+v", obj)
	}
	if !strings.HasPrefix(obj.MediaType, "text/plain") {
		t.Errorf("media type = %q, want sniffed text/plain", obj.MediaType)
	}
	if !e.finalExists(obj.Sum) || len(e.stagingFiles(t)) != 0 {
		t.Error("final file missing or staging file left behind")
	}
	var state string
	var promoted bool
	if err := e.admin.QueryRow(ctx(t), `SELECT state, promoted_at IS NOT NULL FROM studio.blob_uploads WHERE sha256 = $1`, obj.Sum[:]).
		Scan(&state, &promoted); err != nil || state != "promoted" || !promoted {
		t.Errorf("upload = %s promoted_at=%v (%v)", state, promoted, err)
	}
}

// The size cap (approved 256 MiB default, configurable; here 1 MiB): an
// oversized upload is rejected, its staging file removed and the upload
// marked abandoned.
func TestBlobUploadSizeCap(t *testing.T) {
	e := newBlobEnv(t)
	_, err := e.svc.Stage(ctx(t), e.app, bytes.NewReader(make([]byte, testMaxUpload+1)), e.user)
	if !errors.Is(err, blob.ErrTooLarge) {
		t.Fatalf("Stage = %v, want ErrTooLarge", err)
	}
	if len(e.stagingFiles(t)) != 0 {
		t.Error("staging file left after the rejected upload")
	}
	if n := countRows(t, e.admin, `SELECT count(*) FROM studio.blob_uploads WHERE state = 'abandoned'`); n != 1 {
		t.Errorf("abandoned uploads = %d, want 1", n)
	}
	// Exactly at the limit is accepted.
	if _, err := e.svc.Stage(ctx(t), e.app, bytes.NewReader(make([]byte, testMaxUpload)), e.user); err != nil {
		t.Errorf("upload at the limit rejected: %v", err)
	}
}

// B1: crash before transaction 1 commits. A staging file with no row is
// deleted once it is older than 24 hours, and kept before.
func TestB1StagingFileWithoutRow(t *testing.T) {
	e := newBlobEnv(t)
	key := uuid.Must(uuid.NewV7()).String()
	e.writeStaging(t, key, []byte("orphan"))
	if d := e.sweep(t); num(d, "staging_files_removed") != 0 {
		t.Errorf("young orphan removed: %v", d)
	}
	e.later()
	if d := e.sweep(t); num(d, "staging_files_removed") != 1 {
		t.Errorf("sweep details = %v", d)
	}
	if len(e.stagingFiles(t)) != 0 {
		t.Error("orphan staging file survived the sweep")
	}
}

// B2: crash during streaming. After expires_at the file is deleted and the
// upload marked abandoned, with one audit event per sweep.
func TestB2CrashDuringStreaming(t *testing.T) {
	e := newBlobEnv(t)
	id := uuid.Must(uuid.NewV7())
	if _, err := e.app.Exec(ctx(t), `INSERT INTO studio.blob_uploads (id, staging_key, created_by) VALUES ($1, $2, $3)`, id, id.String(), e.user); err != nil {
		t.Fatal(err)
	}
	e.writeStaging(t, id.String(), []byte("partial"))
	if d := e.sweep(t); num(d, "uploads_abandoned") != 0 || e.uploadState(t, id) != "staging" {
		t.Fatalf("unexpired upload swept: %v", d)
	}
	e.later()
	d := e.sweep(t)
	if num(d, "uploads_abandoned") != 1 || e.uploadState(t, id) != "abandoned" || len(e.stagingFiles(t)) != 0 {
		t.Errorf("after expiry: %v, state %s, staging %v", d, e.uploadState(t, id), e.stagingFiles(t))
	}
	if n := countEvents(t, e.admin, `action = 'blob.pending_swept'`); n != 2 {
		t.Errorf("blob.pending_swept events = %d, want one per sweep (2)", n)
	}
}

// B3: crash after the rename, before transaction 2 commits. The final file
// with no row is kept within the grace period, a re-upload of the same
// content reuses it, and an orphan older than the grace period is deleted
// after the per-sha lock and a re-check.
func TestB3FinalFileWithoutRow(t *testing.T) {
	e := newBlobEnv(t)
	orphan := func(data []byte) blob.Sum {
		st, err := e.svc.Stage(ctx(t), e.app, bytes.NewReader(data), e.user)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := e.app.Begin(ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Commit(ctx(t), tx, st, e.user); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(ctx(t)); err != nil { // the crash
			t.Fatal(err)
		}
		if !e.finalExists(st.Sum) || countRows(t, e.admin, `SELECT count(*) FROM studio.blob_objects WHERE sha256 = $1`, st.Sum[:]) != 0 {
			t.Fatal("setup: expected a final file with no row")
		}
		return st.Sum
	}
	reused := orphan([]byte("reused within grace"))
	if d := e.sweep(t); num(d, "final_files_removed") != 0 || !e.finalExists(reused) {
		t.Fatalf("young orphan final removed: %v", d)
	}
	obj := e.upload(t, []byte("reused within grace"))
	if obj.Sum != reused || obj.Deduplicated || !e.finalExists(reused) {
		t.Errorf("re-upload within grace: %+v", obj)
	}

	gone := orphan([]byte("abandoned forever"))
	e.later()
	d := e.sweep(t)
	if num(d, "final_files_removed") != 1 || e.finalExists(gone) {
		t.Errorf("old orphan final: %v, exists %v", d, e.finalExists(gone))
	}
	if !e.finalExists(reused) {
		t.Error("the sweep removed a committed blob's file")
	}
}

// B4: a committed blob whose file is missing or corrupt is detected by
// blob.audit, recorded in blob_health_events, and fails readiness. Nothing
// is repaired, and re-running the audit records nothing new.
func TestB4MissingAndCorruptBlobsDetected(t *testing.T) {
	e := newBlobEnv(t)
	healthy := e.upload(t, []byte("healthy"))
	missing := e.upload(t, []byte("will go missing"))
	corrupt := e.upload(t, []byte("will be corrupted"))
	checker := blob.HealthChecker{Pool: e.app}
	if err := checker.Check(ctx(t)); err != nil {
		t.Fatalf("readiness before damage: %v", err)
	}
	h := missing.Sum.String()
	if err := os.Remove(filepath.Join(e.root, "sha256", h[:2], h[2:4], h)); err != nil {
		t.Fatal(err)
	}
	c := corrupt.Sum.String()
	if err := os.WriteFile(filepath.Join(e.root, "sha256", c[:2], c[2:4], c), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := e.runJob(t, blob.AuditArgs{})
	if num(d, "checked") != 3 || num(d, "missing") != 1 || num(d, "corrupt") != 1 || num(d, "health_events_recorded") != 2 {
		t.Errorf("audit details = %v", d)
	}
	kinds := collect(t, e.admin, `SELECT (o.sha256 = $1 AND h.kind = 'missing' OR o.sha256 = $2 AND h.kind = 'corrupt')::text
		FROM studio.blob_health_events h JOIN studio.blob_objects o ON o.id = h.blob_object_id`, missing.Sum[:], corrupt.Sum[:])
	if strings.Join(kinds, ",") != "true,true" {
		t.Errorf("health events do not match the damage: %v", kinds)
	}
	if err := checker.Check(ctx(t)); err == nil || !strings.Contains(err.Error(), "restore from backup") {
		t.Errorf("readiness with damaged blobs = %v, want a failure", err)
	}
	if d := e.runJob(t, blob.AuditArgs{}); num(d, "health_events_recorded") != 0 {
		t.Errorf("re-run recorded new events: %v", d)
	}
	if !e.finalExists(healthy.Sum) {
		t.Error("healthy blob affected")
	}
}

// B5: the sweeper races an uploader. A commit holding the per-sha lock, with
// its file already renamed into place, blocks the sweep's re-check (seen with
// pg_blocking_pids); after the commit the sweep keeps the file.
func TestB5SweeperRacesUploader(t *testing.T) {
	e := newBlobEnv(t)
	data := []byte("committed while the sweep runs")
	st, err := e.svc.Stage(ctx(t), e.app, bytes.NewReader(data), e.user)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.app.Begin(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := e.svc.Commit(ctx(t), tx, st, e.user); err != nil {
		t.Fatal(err)
	}
	var uploaderPID int32
	if err := tx.QueryRow(ctx(t), `SELECT pg_backend_pid()`).Scan(&uploaderPID); err != nil {
		t.Fatal(err)
	}
	e.later() // the renamed file now looks older than the grace period
	sweepID := e.enqueuePeriodic(t, blob.SweepArgs{})
	waitFor(t, "the sweep to wait on the uploader's per-sha lock", 30*time.Second, func() bool {
		return countRows(t, e.admin, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY (pg_blocking_pids(pid))`, uploaderPID) > 0
	})
	if err := tx.Commit(ctx(t)); err != nil {
		t.Fatal(err)
	}
	e.waitState(t, sweepID, "completed", 30*time.Second)
	if !e.finalExists(st.Sum) || countRows(t, e.admin, `SELECT count(*) FROM studio.blob_objects WHERE sha256 = $1`, st.Sum[:]) != 1 {
		t.Error("the committed blob's file or row is gone")
	}
}

// B6: the sweep is blocked by a backup. While BACKUP_GC is held shared (as
// studioctl backup holds it, M7), the sweep deletes nothing and records that
// it skipped; once released, it runs.
func TestB6SweepSkipsWhileBackupHoldsLock(t *testing.T) {
	e := newBlobEnv(t)
	key := uuid.Must(uuid.NewV7()).String()
	e.writeStaging(t, key, []byte("orphan"))
	e.later()
	backup := e.db.Connect(t, dbroles.Backup)
	if _, err := backup.Exec(ctx(t), `SELECT pg_advisory_lock_shared($1, 0)`, blob.LockNamespaceBackupGC); err != nil {
		t.Fatal(err)
	}
	d := e.sweep(t)
	if d["skipped"] != true || num(d, "staging_files_removed") != 0 || len(e.stagingFiles(t)) != 1 {
		t.Errorf("sweep during backup: %v, staging %v", d, e.stagingFiles(t))
	}
	if _, err := backup.Exec(ctx(t), `SELECT pg_advisory_unlock_shared($1, 0)`, blob.LockNamespaceBackupGC); err != nil {
		t.Fatal(err)
	}
	if d := e.sweep(t); d["skipped"] == true || num(d, "staging_files_removed") != 1 {
		t.Errorf("sweep after backup: %v", d)
	}
}

// B7: concurrent uploads of the same content produce one blob_objects row
// and one final file; one upload is promoted and the rest deduplicated.
func TestB7ConcurrentDedupe(t *testing.T) {
	e := newBlobEnv(t)
	data := []byte("the same content, uploaded eight times at once")
	const n = 8
	var wg sync.WaitGroup
	objs := make([]blob.Object, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			st, err := e.svc.Stage(context.Background(), e.app, bytes.NewReader(data), e.user)
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = db.WithTx(context.Background(), e.app, func(tx pgx.Tx) error {
				var err error
				objs[i], err = e.svc.Commit(context.Background(), tx, st, e.user)
				return err
			})
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}
	ids := map[uuid.UUID]bool{}
	dedup := 0
	for _, o := range objs {
		ids[o.ID] = true
		if o.Deduplicated {
			dedup++
		}
	}
	if len(ids) != 1 || dedup != n-1 {
		t.Errorf("objects: %d distinct ids, %d deduplicated", len(ids), dedup)
	}
	if got := collect(t, e.admin, `SELECT state || ':' || count(*) FROM studio.blob_uploads GROUP BY state`); strings.Join(got, ",") != fmt.Sprintf("deduplicated:%d,promoted:1", n-1) {
		t.Errorf("upload states = %v", got)
	}
	fin, _ := e.store.Finals()
	if len(fin) != 1 || len(e.stagingFiles(t)) != 0 {
		t.Errorf("finals %d, staging %v", len(fin), e.stagingFiles(t))
	}
}

// Database enforcement (plan 5.3, CLAUDE.md: immutability in the database):
// committed blobs and health events can never change, even for the owner,
// and an upload leaves staging exactly once.
func TestBlobLedgerRulesEnforcedInDatabase(t *testing.T) {
	e := newBlobEnv(t)
	obj := e.upload(t, []byte("immutable"))
	if _, err := e.admin.Exec(ctx(t), `INSERT INTO studio.blob_health_events (blob_object_id, kind, created_by) VALUES ($1, 'missing', $2)`,
		obj.ID, workerID); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE studio.blob_objects SET media_type = 'x/y'`,
		`DELETE FROM studio.blob_objects`,
		`TRUNCATE studio.blob_objects CASCADE`,
		`UPDATE studio.blob_health_events SET kind = 'corrupt'`,
		`DELETE FROM studio.blob_health_events`,
		`DELETE FROM studio.blob_uploads`,
		`UPDATE studio.blob_uploads SET state = 'staging'`,
		`UPDATE studio.blob_uploads SET state = 'abandoned'`,
	} {
		wantSQLState(t, exec(t, e.admin, stmt), restrictViolation, "", "owner: "+stmt)
	}
	bad := uuid.Must(uuid.NewV7())
	wantSQLState(t, exec(t, e.admin, `INSERT INTO studio.blob_uploads (id, staging_key, created_by) VALUES ($1, '../etc', $2)`, bad, e.user),
		checkViolation, "blob_uploads_staging_key_is_id", "staging key that is not the id")
	wantSQLState(t, exec(t, e.admin, `INSERT INTO studio.blob_uploads (id, staging_key, state, created_by) VALUES ($1, $2, 'promoted', $3)`, bad, bad.String(), e.user),
		checkViolation, "", "promoted upload without content")
}

// Grants (plan 6.3, X4 as confirmed): each role can do exactly its blob work.
func TestBlobGrants(t *testing.T) {
	e := newBlobEnv(t)
	obj := e.upload(t, []byte("grants")) // the API role's full upload path works
	app := e.db.Connect(t, dbroles.App)
	worker := e.db.Connect(t, dbroles.Worker)
	ro := e.db.Connect(t, dbroles.ReadOnly)
	forbidden := map[*pgx.Conn][]string{
		app: {
			`DELETE FROM studio.blob_uploads`,
			`UPDATE studio.blob_uploads SET expires_at = now()`,
			`SELECT created_by FROM studio.blob_uploads`,
			`INSERT INTO studio.blob_health_events (blob_object_id, kind, created_by) SELECT id, 'missing', created_by FROM studio.blob_objects`,
		},
		worker: {
			`INSERT INTO studio.blob_objects (sha256, size_bytes, media_type, created_by) VALUES (sha256('x'::bytea), 1, 'a/b', '00000000-0000-7000-8000-000000000003')`,
			`SELECT * FROM studio.blob_health_events`,
			`SELECT sha256 FROM studio.blob_uploads`,
			`UPDATE studio.blob_uploads SET sha256 = NULL`,
			`INSERT INTO studio.blob_uploads (id, staging_key, created_by) VALUES ('0190a6e4-0000-7000-8000-0000000000f1', '0190a6e4-0000-7000-8000-0000000000f1', '00000000-0000-7000-8000-000000000003')`,
		},
		ro: {
			`INSERT INTO studio.blob_objects (sha256, size_bytes, media_type, created_by) VALUES (sha256('x'::bytea), 1, 'a/b', '00000000-0000-7000-8000-000000000003')`,
		},
	}
	for conn, stmts := range forbidden {
		for _, stmt := range stmts {
			wantSQLState(t, exec(t, conn, stmt), insufficientPriv, "", stmt)
		}
	}
	// The worker's allowed reads and its health-event insert.
	wantOK(t, exec(t, worker, `SELECT id, sha256 FROM studio.blob_objects`), "worker reads blob_objects")
	wantOK(t, exec(t, worker, `INSERT INTO studio.blob_health_events (blob_object_id, kind, created_by) VALUES ($1, 'corrupt', '00000000-0000-7000-8000-000000000003') ON CONFLICT DO NOTHING`, obj.ID), "worker records a health event")
	wantOK(t, exec(t, ro, `SELECT * FROM studio.blob_uploads`), "readonly reads uploads")
}

// blobDigest summarises the domain state the blob jobs manage: files on disk,
// upload states and health events (not audit events or River rows).
func (e *blobEnv) blobDigest(t *testing.T) func(context.Context) (string, error) {
	return func(c context.Context) (string, error) {
		var parts []string
		st, err := e.store.Staging()
		if err != nil {
			return "", err
		}
		for _, s := range st {
			parts = append(parts, "staging:"+s.Key)
		}
		fin, err := e.store.Finals()
		if err != nil {
			return "", err
		}
		for _, f := range fin {
			parts = append(parts, "final:"+f.Sum.String())
		}
		parts = append(parts, collect(t, e.admin, `SELECT 'upload:' || id || ':' || state FROM studio.blob_uploads`)...)
		parts = append(parts, collect(t, e.admin, `SELECT 'health:' || blob_object_id || ':' || kind FROM studio.blob_health_events`)...)
		sort.Strings(parts)
		return strings.Join(parts, "\n"), c.Err()
	}
}

// blob.sweep is idempotent (plan 12.1, 17.3 #8): a second run over the same
// state changes nothing.
func TestBlobSweepIsIdempotent(t *testing.T) {
	e := newBlobEnv(t)
	e.upload(t, []byte("kept"))
	e.writeStaging(t, uuid.Must(uuid.NewV7()).String(), []byte("orphan"))
	e.later()
	if err := jobstest.CheckIdempotent(ctx(t), func(context.Context) error { e.sweep(t); return nil }, e.blobDigest(t)); err != nil {
		t.Error(err)
	}
}

// blob.audit is idempotent: re-detecting damage records nothing new.
func TestBlobAuditIsIdempotent(t *testing.T) {
	e := newBlobEnv(t)
	obj := e.upload(t, []byte("goes missing"))
	h := obj.Sum.String()
	if err := os.Remove(filepath.Join(e.root, "sha256", h[:2], h[2:4], h)); err != nil {
		t.Fatal(err)
	}
	if err := jobstest.CheckIdempotent(ctx(t), func(context.Context) error { e.runJob(t, blob.AuditArgs{}); return nil }, e.blobDigest(t)); err != nil {
		t.Error(err)
	}
}
