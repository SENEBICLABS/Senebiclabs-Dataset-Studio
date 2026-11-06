//go:build integration

package integration

// M7 backup, restore and verify-restore (plan 13, 16.2): C1-C8 and C10 on
// the existing tables, B8 (C1 on the blob protocol) and P6. C9 needs release
// bundles and belongs to M13.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/backup"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob/fsstore"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db/migrate"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/testdb"
)

func pgTools(t *testing.T) backup.Tools {
	t.Helper()
	c := os.Getenv("STUDIO_PG_CONTAINER")
	if c == "" {
		t.Fatal("STUDIO_PG_CONTAINER is not set; run `make test-integration` (it passes the compose PostgreSQL container)")
	}
	return backup.DockerTools{Container: c}
}

func (e *blobEnv) takeBackup(t *testing.T, hook func(context.Context)) (string, backup.Manifest) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "backup")
	m, err := backup.Backup(ctx(t), backup.Config{
		BackupURL: e.db.URL(dbroles.Backup), BlobRoot: e.root, Dir: dir, Tools: pgTools(t), AfterSnapshot: hook,
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	return dir, m
}

type restoreTarget struct {
	name string
	root string
	cfg  backup.RestoreConfig
}

func newTarget(t *testing.T, dir string) restoreTarget {
	t.Helper()
	name := testdb.ReserveName(t)
	root := filepath.Join(t.TempDir(), "restored-blobs")
	return restoreTarget{name: name, root: root, cfg: backup.RestoreConfig{
		Dir:          dir,
		BootstrapURL: testdb.AdminMaintenanceURL(t),
		MigratorURL:  testdb.URLForDatabase(t, dbroles.Migrator, name),
		BackupURL:    testdb.URLForDatabase(t, dbroles.Backup, name),
		AppURL:       testdb.URLForDatabase(t, dbroles.App, name),
		BlobRoot:     root,
		Tools:        pgTools(t),
		OSUser:       "tester",
		Hostname:     "test-host",
	}}
}

func (r restoreTarget) exists(t *testing.T) bool {
	t.Helper()
	conn, err := pgx.Connect(ctx(t), testdb.AdminMaintenanceURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var ok bool
	if err := conn.QueryRow(ctx(t), `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, r.name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// seed writes a little of everything the backup must carry.
func (e *blobEnv) seed(t *testing.T) {
	t.Helper()
	for i := range 3 {
		e.upload(t, fmt.Appendf(nil, "seed blob %d", i))
	}
	e.sweep(t) // a job_execution event and a completed River job
}

// The round trip and P6: a backup restores into a new database and blob
// root; verify-restore passes; the installation id is kept; the grants of the
// restored database have no drift; and one system.restored event is written
// by system:cli.
func TestBackupRestoreRoundTripAndP6(t *testing.T) {
	e := newBlobEnv(t)
	e.seed(t)
	var srcInstallation string
	if err := e.admin.QueryRow(ctx(t), `SELECT installation_id::text FROM studio.installation`).Scan(&srcInstallation); err != nil {
		t.Fatal(err)
	}
	dir, m := e.takeBackup(t, nil)
	if len(m.Blobs) != 3 || m.InstallationID != srcInstallation || m.NewestAuditEventID == "" {
		t.Fatalf("manifest = %d blobs, installation %s, newest event %q", len(m.Blobs), m.InstallationID, m.NewestAuditEventID)
	}
	latest, _ := migrate.LatestVersion()
	if m.GooseVersion != latest || !strings.Contains(m.PgDumpVersion, "PostgreSQL) 18.") {
		t.Errorf("manifest schema %d (want %d), pg_dump %q", m.GooseVersion, latest, m.PgDumpVersion)
	}
	if _, err := backup.Verify(dir); err != nil {
		t.Fatalf("verify: %v", err)
	}

	target := newTarget(t, dir)
	res, err := backup.Restore(ctx(t), target.cfg)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored := &testdb.DB{Name: target.name}
	radmin := restored.Connect(t, testdb.Admin)
	var installation string
	if err := radmin.QueryRow(ctx(t), `SELECT installation_id::text FROM studio.installation`).Scan(&installation); err != nil || installation != srcInstallation {
		t.Errorf("P6: restored installation %q (%v), want %s", installation, err, srcInstallation)
	}
	var actor, action string
	var details []byte
	if err := radmin.QueryRow(ctx(t), `SELECT actor_principal_id::text, action, details FROM studio.audit_events WHERE id = $1`, res.EventID).
		Scan(&actor, &action, &details); err != nil {
		t.Fatalf("system.restored event: %v", err)
	}
	var d audit.SystemRestoredDetails
	if err := json.Unmarshal(details, &d); err != nil {
		t.Fatal(err)
	}
	if actor != principals.CLIID || action != audit.ActionSystemRestored || d.OSUser != "tester" || d.InstallationID != srcInstallation || d.Blobs != 3 {
		t.Errorf("event = %s by %s: %s", action, actor, details)
	}
	if n := countRows(t, radmin, `SELECT count(*) FROM studio.audit_events WHERE action = 'system.restored'`); n != 1 {
		t.Errorf("system.restored events = %d, want 1", n)
	}
	// Restored history keeps its original times: COPY runs before the
	// triggers that assign occurred_at are created.
	if n := countRows(t, radmin, `SELECT count(*) FROM studio.audit_events WHERE id::text = $1`, m.NewestAuditEventID); n != 1 {
		t.Errorf("the newest backed-up event is missing after restore")
	}
	fin, err := mustStore(t, target.root).Finals()
	if err != nil || len(fin) != 3 {
		t.Errorf("restored blobs = %d (%v), want 3", len(fin), err)
	}
	assertNoGrantDrift(t, restored)
	// The restored database is usable by the API role.
	if err := (blob.HealthChecker{Pool: poolFor(t, testdb.URLForDatabase(t, dbroles.App, target.name))}).Check(ctx(t)); err != nil {
		t.Errorf("restored readiness: %v", err)
	}
}

func mustStore(t *testing.T, root string) *fsstore.Store {
	t.Helper()
	s, err := fsstore.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// concurrentBackups is C1 and B8: writes keep running while a backup is
// taken; the backup and its restore must be exactly one consistent
// snapshot. Each iteration restores and runs verify-restore. The writer is
// paced and capped (maxWritesPerBackup) so the store does not grow without
// bound across iterations; it still writes before and during the dump.
const maxWritesPerBackup = 25

func concurrentBackups(t *testing.T, iterations int, write func(e *blobEnv, i int) error) {
	e := newBlobEnv(t)
	e.seed(t)
	for it := range iterations {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var writes atomic.Int64
		var werr atomic.Value
		hook := func(context.Context) {
			// Start writing once the snapshot exists, so writes land both
			// before the dump starts and while it runs.
			wg.Go(func() {
				for i := 0; i < maxWritesPerBackup; i++ {
					select {
					case <-stop:
						return
					case <-time.After(5 * time.Millisecond):
					}
					if err := write(e, it*100000+i); err != nil {
						werr.Store(err)
						return
					}
					writes.Add(1)
				}
			})
			time.Sleep(50 * time.Millisecond)
		}
		dir, _ := e.takeBackup(t, hook)
		close(stop)
		wg.Wait()
		if err, _ := werr.Load().(error); err != nil {
			t.Fatalf("iteration %d: concurrent write: %v", it, err)
		}
		if writes.Load() == 0 {
			t.Fatalf("iteration %d: no write ran during the backup", it)
		}
		target := newTarget(t, dir)
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		_, err := backup.Restore(rctx, target.cfg)
		cancel()
		if err != nil {
			t.Fatalf("iteration %d (%d concurrent writes): restore: %v", it, writes.Load(), err)
		}
	}
}

// C1: concurrent writes during backup, repeated 20 times.
func TestC1ConcurrentWritesDuringBackup(t *testing.T) {
	w := audit.NewWriter(m6Catalog)
	concurrentBackups(t, 20, func(e *blobEnv, i int) error {
		_, err := w.Record(context.Background(), e.app, audit.Event{
			Action: m6Requested, Outcome: audit.Succeeded, ActorPrincipalID: userU, ActorKind: audit.User,
			Details: map[string]int{"n": i},
		})
		return err
	})
}

// B8: C1 re-run on the blob protocol: uploads (both transactions) keep
// committing while the backup runs; every restored blob row has its file.
func TestB8ConcurrentUploadsDuringBackup(t *testing.T) {
	concurrentBackups(t, 20, func(e *blobEnv, i int) error {
		st, err := e.svc.Stage(context.Background(), e.app, bytes.NewReader(fmt.Appendf(nil, "upload %d", i)), e.user)
		if err != nil {
			return err
		}
		tx, err := e.app.Begin(context.Background())
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := e.svc.Commit(context.Background(), tx, st, e.user); err != nil {
			return err
		}
		return tx.Commit(context.Background())
	})
}

// C2: an upload still staging at snapshot time is not in the manifest, and
// after restore it is unreferenced: its row is still staging, no blob row or
// file exists for it.
func TestC2StagingUploadIsNotBackedUp(t *testing.T) {
	e := newBlobEnv(t)
	e.seed(t)
	st, err := e.svc.Stage(ctx(t), e.app, bytes.NewReader([]byte("still staging at snapshot time")), e.user)
	if err != nil {
		t.Fatal(err)
	}
	dir, m := e.takeBackup(t, nil)
	for _, b := range m.Blobs {
		if b.SHA256 == st.Sum.String() {
			t.Fatal("the staging upload's content is in the manifest")
		}
	}
	target := newTarget(t, dir)
	if _, err := backup.Restore(ctx(t), target.cfg); err != nil {
		t.Fatal(err)
	}
	radmin := (&testdb.DB{Name: target.name}).Connect(t, testdb.Admin)
	if n := countRows(t, radmin, `SELECT count(*) FROM studio.blob_objects WHERE sha256 = $1`, st.Sum[:]); n != 0 {
		t.Error("restored blob_objects references the staging upload")
	}
	var state string
	if err := radmin.QueryRow(ctx(t), `SELECT state FROM studio.blob_uploads WHERE id = $1`, st.UploadID).Scan(&state); err != nil || state != "staging" {
		t.Errorf("restored upload state %q (%v), want staging", state, err)
	}
	if st, _ := mustStore(t, target.root).Staging(); len(st) != 0 {
		t.Errorf("restored blob root has staging files: %v", st)
	}
}

// C3: a failed blob copy leaves no COMPLETE marker.
func TestC3FailedBlobCopyLeavesNoCompleteMarker(t *testing.T) {
	e := newBlobEnv(t)
	obj := e.upload(t, []byte("this file disappears"))
	h := obj.Sum.String()
	if err := os.Remove(filepath.Join(e.root, "sha256", h[:2], h[2:4], h)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backup")
	_, err := backup.Backup(ctx(t), backup.Config{BackupURL: e.db.URL(dbroles.Backup), BlobRoot: e.root, Dir: dir, Tools: pgTools(t)})
	if err == nil {
		t.Fatal("backup succeeded with a missing blob")
	}
	if _, err := os.Stat(filepath.Join(dir, backup.CompleteFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("COMPLETE exists after a failed backup (%v)", err)
	}
	if _, err := backup.Verify(dir); err == nil {
		t.Error("an incomplete backup verified")
	}
}

// C4: a corrupt or missing blob in the backup is caught by the restore
// pre-check, before anything is touched.
func TestC4DamagedBackupBlobCaughtBeforeRestore(t *testing.T) {
	e := newBlobEnv(t)
	e.seed(t)
	for _, damage := range []string{"corrupt", "missing"} {
		t.Run(damage, func(t *testing.T) {
			dir, m := e.takeBackup(t, nil)
			h := m.Blobs[0].SHA256
			p := filepath.Join(dir, backup.BlobsDir, "sha256", h[:2], h[2:4], h)
			var err error
			if damage == "corrupt" {
				err = os.WriteFile(p, []byte("tampered"), 0o600)
			} else {
				err = os.Remove(p)
			}
			if err != nil {
				t.Fatal(err)
			}
			target := newTarget(t, dir)
			if _, err := backup.Restore(ctx(t), target.cfg); err == nil || !strings.Contains(err.Error(), "in the backup") {
				t.Errorf("restore = %v, want the pre-check to refuse", err)
			}
			if target.exists(t) {
				t.Error("the target database was created before the pre-check finished")
			}
		})
	}
}

// C5: tampering is detected: a changed dump, a changed manifest, a removed
// COMPLETE marker, and an unlisted extra blob are all refused before
// anything is touched.
func TestC5TamperingDetected(t *testing.T) {
	e := newBlobEnv(t)
	e.seed(t)
	cases := map[string]func(dir string) error{
		"dump changed": func(dir string) error {
			p := filepath.Join(dir, backup.DumpFile)
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			b[len(b)/2] ^= 0xff
			return os.WriteFile(p, b, 0o600) //nolint:gosec // G703: C5 tampers with its own backup in t.TempDir().
		},
		"manifest changed": func(dir string) error {
			p := filepath.Join(dir, backup.ManifestFile)
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			tampered := bytes.Replace(b, []byte(`"format_version": 1`), []byte(`"format_version": 1 `), 1)
			return os.WriteFile(p, tampered, 0o600) //nolint:gosec // G703: C5 tampers with its own backup in t.TempDir().
		},
		"COMPLETE removed": func(dir string) error { return os.Remove(filepath.Join(dir, backup.CompleteFile)) },
		"extra blob": func(dir string) error {
			s, err := fsstore.New(filepath.Join(dir, backup.BlobsDir))
			if err != nil {
				return err
			}
			key := uuid.Must(uuid.NewV7()).String()
			w, err := s.CreateStaging(key)
			if err != nil {
				return err
			}
			_, _ = w.Write([]byte("smuggled"))
			if err := w.Close(); err != nil {
				return err
			}
			return s.Promote(key, sha256.Sum256([]byte("smuggled")))
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := e.takeBackup(t, nil)
			if err := tamper(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := backup.Verify(dir); err == nil {
				t.Error("verify accepted the tampered backup")
			}
			target := newTarget(t, dir)
			if _, err := backup.Restore(ctx(t), target.cfg); err == nil {
				t.Error("restore accepted the tampered backup")
			}
			if target.exists(t) {
				t.Error("the target database was touched")
			}
		})
	}
}

// C6: restoring into a non-empty target is refused, for the database and
// for the blob root.
func TestC6NonEmptyTargetRefused(t *testing.T) {
	e := newBlobEnv(t)
	e.seed(t)
	dir, _ := e.takeBackup(t, nil)

	occupied := testdb.New(t) // a migrated database
	target := newTarget(t, dir)
	cfg := target.cfg
	cfg.MigratorURL = occupied.URL(dbroles.Migrator)
	cfg.BackupURL = occupied.URL(dbroles.Backup)
	cfg.AppURL = occupied.URL(dbroles.App)
	if _, err := backup.Restore(ctx(t), cfg); err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Errorf("restore into a migrated database = %v, want refusal", err)
	}

	target = newTarget(t, dir)
	if err := os.MkdirAll(target.root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target.root, "existing"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Restore(ctx(t), target.cfg); err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Errorf("restore into a non-empty blob root = %v, want refusal", err)
	}
	if target.exists(t) {
		t.Error("the target database was created")
	}
}

// C7: a backup from a newer schema is refused. The manifest is rewritten as
// a genuine newer backup would carry it, with a matching COMPLETE marker, so
// the refusal comes from the version check and not from tamper detection.
func TestC7NewerSchemaRefused(t *testing.T) {
	e := newBlobEnv(t)
	dir, m := e.takeBackup(t, nil)
	latest, _ := migrate.LatestVersion()
	m.GooseVersion = latest + 1
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	sum := sha256.Sum256(raw)
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.CompleteFile), []byte(hex.EncodeToString(sum[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Verify(dir); err != nil {
		t.Fatalf("setup: the rewritten backup should verify: %v", err)
	}
	target := newTarget(t, dir)
	if _, err := backup.Restore(ctx(t), target.cfg); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("restore = %v, want refusal of a newer schema", err)
	}
	if target.exists(t) {
		t.Error("the target database was touched")
	}
}

// C8: blob.sweep is blocked by a running backup's BACKUP_GC lock.
func TestC8SweepBlockedByBackupLock(t *testing.T) {
	e := newBlobEnv(t)
	e.writeStaging(t, uuid.Must(uuid.NewV7()).String(), []byte("orphan"))
	e.later()
	var during map[string]any
	e.takeBackup(t, func(context.Context) { during = e.sweep(t) })
	if during["skipped"] != true || num(during, "staging_files_removed") != 0 {
		t.Errorf("sweep during the backup = %v, want skipped", during)
	}
	if d := e.sweep(t); d["skipped"] == true || num(d, "staging_files_removed") != 1 {
		t.Errorf("sweep after the backup = %v", d)
	}
}

// C10: a reference to a nonexistent blob is rejected by the database.
func TestC10ReferenceToMissingBlobRejected(t *testing.T) {
	e := newBlobEnv(t)
	wantSQLState(t, exec(t, e.admin, `INSERT INTO studio.blob_health_events (blob_object_id, kind, created_by) VALUES ($1, 'missing', $2)`,
		uuid.Must(uuid.NewV7()), workerID), foreignKeyViolation, "blob_health_events_blob_fkey", "health event for a nonexistent blob")
}

// verify-restore detects a restored blob that is missing and fails, naming
// it. It writes no health event: no role used by restore holds INSERT on
// blob_health_events in the plan's matrix (6.3); blob.audit records it.
func TestVerifyRestoreDetectsMissingBlobs(t *testing.T) {
	e := newBlobEnv(t)
	e.seed(t)
	dir, m := e.takeBackup(t, nil)
	target := newTarget(t, dir)
	if _, err := backup.Restore(ctx(t), target.cfg); err != nil {
		t.Fatal(err)
	}
	h := m.Blobs[0].SHA256
	if err := os.Remove(filepath.Join(target.root, "sha256", h[:2], h[2:4], h)); err != nil {
		t.Fatal(err)
	}
	err := backup.VerifyRestore(ctx(t), m, target.cfg.BackupURL, mustStore(t, target.root))
	if err == nil || !strings.Contains(err.Error(), "restored blob "+h+" is missing") {
		t.Errorf("verify-restore = %v, want a failure naming the missing blob", err)
	}
	radmin := (&testdb.DB{Name: target.name}).Connect(t, testdb.Admin)
	if n := countRows(t, radmin, `SELECT count(*) FROM studio.blob_health_events`); n != 0 {
		t.Errorf("verify-restore wrote %d health events; it holds no grant for that", n)
	}
}

func poolFor(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(ctx(t), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}
