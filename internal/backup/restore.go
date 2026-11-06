package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob/fsstore"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/bootstrap"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db/migrate"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals"
)

// RestoreConfig configures a restore. Every URL except BootstrapURL names the
// target database; each role does only its own step (plan 6.2).
type RestoreConfig struct {
	Dir          string // the backup directory
	BootstrapURL string // superuser, maintenance database: creates the empty target (step 2)
	MigratorURL  string // pg_restore and grants (steps 3, 4)
	BackupURL    string // verify-restore reads (step 5)
	AppURL       string // verify-restore's write step and system.restored (steps 5, 6)
	BlobRoot     string // the target blob root; must not exist or be empty
	Tools        Tools
	OSUser       string // recorded in system.restored (plan 7.2)
	Hostname     string
}

// Result reports a completed restore.
type Result struct {
	Manifest Manifest
	EventID  string
}

// Verify checks a backup directory without touching any database: the
// COMPLETE marker, the manifest, the dump hash and every blob's hash
// (plan 13, restore step 1).
func Verify(dir string) (Manifest, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return Manifest{}, err
	}
	sum, size, err := fileSHA256(filepath.Join(dir, DumpFile))
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: read dump: %w", err)
	}
	if sum != m.Dump.SHA256 || size != m.Dump.Size {
		return Manifest{}, errors.New("backup: the dump does not match the manifest: the backup was altered")
	}
	store, err := openExistingStore(filepath.Join(dir, BlobsDir))
	if err != nil {
		return Manifest{}, err
	}
	listed := map[blob.Sum]bool{}
	for _, b := range m.Blobs {
		s, err := blob.ParseSum(b.SHA256)
		if err != nil {
			return Manifest{}, err
		}
		listed[s] = true
		h, err := store.Verify(s)
		if err != nil {
			return Manifest{}, err
		}
		if h != blob.Healthy {
			return Manifest{}, fmt.Errorf("backup: blob %s is %s in the backup", s, h)
		}
	}
	finals, err := store.Finals()
	if err != nil {
		return Manifest{}, err
	}
	for _, f := range finals {
		if !listed[f.Sum] {
			return Manifest{}, fmt.Errorf("backup: blob %s is in the backup but not in the manifest", f.Sum)
		}
	}
	return m, nil
}

// Restore restores a backup into a new or empty database and blob root
// (plan 13). Nothing is touched until every hash and the target have been
// checked.
func Restore(ctx context.Context, cfg RestoreConfig) (Result, error) {
	if cfg.Tools == nil {
		return Result{}, errors.New("backup: tools are required")
	}
	// Step 1: verify everything first.
	m, err := Verify(cfg.Dir)
	if err != nil {
		return Result{}, err
	}
	latest, err := migrate.LatestVersion()
	if err != nil {
		return Result{}, err
	}
	if m.GooseVersion > latest {
		return Result{}, fmt.Errorf("backup: the backup is at schema version %d, newer than this build's %d", m.GooseVersion, latest)
	}
	target, err := sameTarget(cfg)
	if err != nil {
		return Result{}, err
	}
	if _, err := cfg.Tools.Version(ctx); err != nil {
		return Result{}, err
	}
	if err := requireEmptyBlobRoot(cfg.BlobRoot); err != nil {
		return Result{}, err
	}
	admin, err := pgx.Connect(ctx, cfg.BootstrapURL)
	if err != nil {
		return Result{}, fmt.Errorf("backup: connect as the bootstrap superuser: %w", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(ctx)) }()
	if err := requireEmptyDatabase(ctx, admin, cfg.MigratorURL, target); err != nil {
		return Result{}, err
	}

	// Step 2: the empty target database, owned by studio_owner.
	if err := bootstrap.EnsureDatabase(ctx, admin, target, ""); err != nil {
		return Result{}, fmt.Errorf("backup: create target database: %w", err)
	}

	// Step 3: pg_restore as the migrator, in one transaction.
	migrator, err := ConnFromURL(cfg.MigratorURL)
	if err != nil {
		return Result{}, err
	}
	dumpFile, err := os.Open(filepath.Join(cfg.Dir, DumpFile))
	if err != nil {
		return Result{}, err
	}
	err = cfg.Tools.Restore(ctx, migrator, dumpFile)
	_ = dumpFile.Close()
	if err != nil {
		return Result{}, err
	}

	// Step 4: grants, then the blobs, verified as they are copied.
	if err := migrate.ReapplyGrants(ctx, cfg.MigratorURL); err != nil {
		return Result{}, fmt.Errorf("backup: re-apply grants: %w", err)
	}
	src, err := openExistingStore(filepath.Join(cfg.Dir, BlobsDir))
	if err != nil {
		return Result{}, err
	}
	dst, err := fsstore.New(cfg.BlobRoot)
	if err != nil {
		return Result{}, err
	}
	for _, b := range m.Blobs {
		sum, _ := blob.ParseSum(b.SHA256)
		if err := copyBlob(src, dst, sum, b.Size); err != nil {
			return Result{}, err
		}
	}

	// Step 5: verify-restore.
	if err := VerifyRestore(ctx, m, cfg.BackupURL, dst); err != nil {
		return Result{}, err
	}

	// Step 6: the system.restored event.
	id, err := writeRestoredEvent(ctx, cfg, m)
	if err != nil {
		return Result{}, err
	}
	return Result{Manifest: m, EventID: id}, nil
}

// VerifyRestore checks a restored database and blob root against the
// manifest (plan 13, step 5), reading as studio_backup: the same tables with
// the same row counts, the same schema version and installation, exactly the
// manifest's blobs, and every blob present with the right hash. A missing or
// corrupt blob fails verification, naming each one, so the restore stops
// before its system.restored event. It does not write blob_health_events:
// the plan's grant matrix (6.3) gives no role used by restore INSERT on that
// table; blob.audit (worker) records such damage on its next run. Registry
// and release-manifest hashes have no tables before M8/M13.
func VerifyRestore(ctx context.Context, m Manifest, backupURL string, store blob.Store) error {
	db, err := pgx.Connect(ctx, backupURL)
	if err != nil {
		return fmt.Errorf("verify-restore: connect as studio_backup: %w", err)
	}
	defer func() { _ = db.Close(context.WithoutCancel(ctx)) }()

	var problems []error
	counts, err := rowCounts(ctx, db)
	if err != nil {
		return err
	}
	if diff := compareCounts(m.RowCounts, counts); diff != "" {
		problems = append(problems, fmt.Errorf("row counts differ: %s", diff))
	}
	var goose int64
	var installation string
	if err := db.QueryRow(ctx, `SELECT coalesce(max(version_id), 0) FROM studio.goose_db_version WHERE is_applied`).Scan(&goose); err != nil {
		return fmt.Errorf("verify-restore: goose version: %w", err)
	}
	if goose != m.GooseVersion {
		problems = append(problems, fmt.Errorf("schema version %d, manifest %d", goose, m.GooseVersion))
	}
	if err := db.QueryRow(ctx, `SELECT installation_id::text FROM studio.installation`).Scan(&installation); err != nil {
		return fmt.Errorf("verify-restore: installation: %w", err)
	}
	if installation != m.InstallationID {
		problems = append(problems, fmt.Errorf("installation %s, manifest %s", installation, m.InstallationID))
	}

	rows, err := db.Query(ctx, `SELECT id, sha256, size_bytes FROM studio.blob_objects ORDER BY sha256`)
	if err != nil {
		return fmt.Errorf("verify-restore: blobs: %w", err)
	}
	type row struct {
		id   uuid.UUID
		sum  blob.Sum
		size int64
	}
	var got []row
	for rows.Next() {
		var r row
		var raw []byte
		if err := rows.Scan(&r.id, &raw, &r.size); err != nil {
			return err
		}
		if r.sum, err = blob.SumFromBytes(raw); err != nil {
			return err
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	want := make([]BlobEntry, 0, len(got))
	for _, r := range got {
		want = append(want, BlobEntry{SHA256: r.sum.String(), Size: r.size})
	}
	if !slices.Equal(want, m.Blobs) {
		problems = append(problems, errors.New("blob_objects differs from the manifest's blob list"))
	}
	for _, r := range got {
		h, err := store.Verify(r.sum)
		if err != nil {
			return err
		}
		if h != blob.Healthy {
			problems = append(problems, fmt.Errorf("restored blob %s is %s", r.sum, h))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("verify-restore failed: %w", errors.Join(problems...))
	}
	return nil
}

func compareCounts(want, got map[string]int64) string {
	var diffs []string
	for t, w := range want {
		if g, ok := got[t]; !ok {
			diffs = append(diffs, t+" missing")
		} else if g != w {
			diffs = append(diffs, fmt.Sprintf("%s %d≠%d", t, g, w))
		}
	}
	for t := range got {
		if _, ok := want[t]; !ok {
			diffs = append(diffs, t+" unexpected")
		}
	}
	sort.Strings(diffs)
	if len(diffs) == 0 {
		return ""
	}
	return fmt.Sprint(diffs)
}

func writeRestoredEvent(ctx context.Context, cfg RestoreConfig, m Manifest) (string, error) {
	app, err := pgx.Connect(ctx, cfg.AppURL)
	if err != nil {
		return "", fmt.Errorf("backup: connect as studio_app: %w", err)
	}
	defer func() { _ = app.Close(context.WithoutCancel(ctx)) }()
	raw, err := os.ReadFile(filepath.Join(cfg.Dir, ManifestFile))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return audit.NewWriter(audit.Actions).Record(ctx, app, audit.Event{
		Action: audit.ActionSystemRestored, Outcome: audit.Succeeded,
		ActorPrincipalID: principals.CLIID, ActorKind: audit.System,
		TargetType: "installation", TargetID: m.InstallationID,
		Details: audit.SystemRestoredDetails{
			BackupCreatedAt: m.CreatedAt.Format(time.RFC3339Nano), ManifestSHA256: hex.EncodeToString(sum[:]),
			InstallationID: m.InstallationID, GooseVersion: m.GooseVersion, Blobs: len(m.Blobs),
			OSUser: cfg.OSUser, Hostname: cfg.Hostname,
		},
	})
}

// sameTarget requires every target URL to name the same database.
func sameTarget(cfg RestoreConfig) (string, error) {
	var name string
	for _, u := range []string{cfg.MigratorURL, cfg.BackupURL, cfg.AppURL} {
		c, err := ConnFromURL(u)
		if err != nil {
			return "", err
		}
		if name != "" && c.Database != name {
			return "", errors.New("backup: the migrator, backup and app URLs must name the same target database")
		}
		name = c.Database
	}
	return name, nil
}

// requireEmptyDatabase refuses a target that holds any non-system schema or
// any relation (plan 13: refuse a non-empty target). A target that does not
// exist yet is fine.
func requireEmptyDatabase(ctx context.Context, admin *pgx.Conn, migratorURL, name string) error {
	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return fmt.Errorf("backup: check target: %w", err)
	}
	if !exists {
		return nil
	}
	cfg, err := pgx.ParseConfig(migratorURL)
	if err != nil {
		return errors.New("backup: invalid migrator URL")
	}
	acfg := admin.Config().Copy()
	acfg.Database = cfg.Database
	conn, err := pgx.ConnectConfig(ctx, acfg)
	if err != nil {
		return fmt.Errorf("backup: inspect target: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var objects int
	if err := conn.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM pg_namespace
		        WHERE nspname NOT IN ('public', 'information_schema') AND nspname NOT LIKE 'pg\_%')
		     + (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public')`).
		Scan(&objects); err != nil {
		return fmt.Errorf("backup: inspect target: %w", err)
	}
	if objects > 0 {
		return fmt.Errorf("backup: target database %q is not empty", name)
	}
	return nil
}

func requireEmptyBlobRoot(root string) error {
	if !filepath.IsAbs(root) {
		return errors.New("backup: the target blob root must be an absolute path")
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("backup: read target blob root: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("backup: target blob root %s is not empty", root)
	}
	return nil
}

// openExistingStore opens the blob store inside a backup without creating it.
func openExistingStore(root string) (*fsstore.Store, error) {
	if _, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("backup: blob directory: %w", err)
	}
	return fsstore.New(root)
}
