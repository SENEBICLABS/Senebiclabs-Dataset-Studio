package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob/fsstore"
)

// Config configures a backup.
type Config struct {
	// BackupURL connects as studio_backup to the database to back up.
	BackupURL string
	// BlobRoot is the live blob store's root.
	BlobRoot string
	// Dir is the new backup directory. It must not exist or be empty.
	Dir   string
	Tools Tools
	// AfterSnapshot, if set, runs once the BACKUP_GC lock is held and the
	// snapshot exported, before the dump. Tests use it to write concurrently
	// (C1, B8) and to hold the backup open (C8).
	AfterSnapshot func(ctx context.Context)
}

// Backup takes a consistent backup (plan 13): it holds BACKUP_GC shared for
// the whole run, so blob.sweep cannot delete anything meanwhile; reads the
// manifest facts, dumps and lists blobs from one exported snapshot; copies
// and re-hashes every listed blob; and writes manifest.json and then
// COMPLETE. Any failure leaves no COMPLETE marker.
func Backup(ctx context.Context, cfg Config) (Manifest, error) {
	if cfg.Tools == nil || cfg.Dir == "" || cfg.BlobRoot == "" {
		return Manifest{}, errors.New("backup: tools, a directory and the blob root are required")
	}
	conn, err := ConnFromURL(cfg.BackupURL)
	if err != nil {
		return Manifest{}, err
	}
	pgDump, err := cfg.Tools.Version(ctx)
	if err != nil {
		return Manifest{}, err
	}
	if err := prepareEmptyDir(cfg.Dir); err != nil {
		return Manifest{}, err
	}
	src, err := fsstore.New(cfg.BlobRoot)
	if err != nil {
		return Manifest{}, err
	}
	dst, err := fsstore.New(filepath.Join(cfg.Dir, BlobsDir))
	if err != nil {
		return Manifest{}, err
	}

	db, err := pgx.Connect(ctx, cfg.BackupURL)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: connect as studio_backup: %w", err)
	}
	defer func() { _ = db.Close(context.WithoutCancel(ctx)) }()

	// Step 1: BACKUP_GC shared, held by this session until it closes.
	if _, err := db.Exec(ctx, `SELECT pg_advisory_lock_shared($1, 0)`, blob.LockNamespaceBackupGC); err != nil {
		return Manifest{}, fmt.Errorf("backup: BACKUP_GC: %w", err)
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: begin snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var snapshot string
	if err := tx.QueryRow(ctx, `SELECT pg_export_snapshot()`).Scan(&snapshot); err != nil {
		return Manifest{}, fmt.Errorf("backup: export snapshot: %w", err)
	}

	// Step 2: the manifest facts, from the same snapshot.
	m, err := readSnapshotFacts(ctx, tx)
	if err != nil {
		return Manifest{}, err
	}
	m.FormatVersion, m.Database, m.PgDumpVersion = FormatVersion, conn.Database, pgDump
	m.CreatedAt = time.Now().UTC()
	m.ReleaseManifestHashes = []string{}
	if cfg.AfterSnapshot != nil {
		cfg.AfterSnapshot(ctx)
	}

	// Step 3: pg_dump of exactly that snapshot, inside the container.
	m.Dump, err = dump(ctx, cfg, conn, snapshot)
	if err != nil {
		return Manifest{}, err
	}

	// Step 4: copy and re-hash every listed blob; any mismatch aborts.
	for _, b := range m.Blobs {
		sum, err := blob.ParseSum(b.SHA256)
		if err != nil {
			return Manifest{}, err
		}
		if err := copyBlob(src, dst, sum, b.Size); err != nil {
			return Manifest{}, err
		}
	}

	// Step 5: manifest.json, then COMPLETE last.
	if err := writeManifest(cfg.Dir, m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func readSnapshotFacts(ctx context.Context, tx pgx.Tx) (Manifest, error) {
	m := Manifest{RowCounts: map[string]int64{}}
	counts, err := rowCounts(ctx, tx)
	if err != nil {
		return m, err
	}
	m.RowCounts = counts
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version_id), 0) FROM studio.goose_db_version WHERE is_applied`).
		Scan(&m.GooseVersion); err != nil {
		return m, fmt.Errorf("backup: goose version: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT installation_id::text FROM studio.installation`).Scan(&m.InstallationID); err != nil {
		return m, fmt.Errorf("backup: installation: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT id::text FROM studio.audit_events ORDER BY occurred_at DESC, id DESC LIMIT 1`).
		Scan(&m.NewestAuditEventID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return m, fmt.Errorf("backup: newest audit event: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT sha256, size_bytes FROM studio.blob_objects ORDER BY sha256`)
	if err != nil {
		return m, fmt.Errorf("backup: list blobs: %w", err)
	}
	m.Blobs = []BlobEntry{}
	for rows.Next() {
		var sum []byte
		var size int64
		if err := rows.Scan(&sum, &size); err != nil {
			return m, err
		}
		m.Blobs = append(m.Blobs, BlobEntry{SHA256: hex.EncodeToString(sum), Size: size})
	}
	return m, rows.Err()
}

// rowCounts counts the rows of every table in the studio schema.
func rowCounts(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}) (map[string]int64, error) {
	rows, err := q.Query(ctx, `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'studio' AND c.relkind IN ('r', 'p') ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("backup: list tables: %w", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(tables))
	for _, t := range tables {
		var n int64
		if err := q.QueryRow(ctx, `SELECT count(*) FROM studio.`+pgx.Identifier{t}.Sanitize()).Scan(&n); err != nil {
			return nil, fmt.Errorf("backup: count %s: %w", t, err)
		}
		out[t] = n
	}
	return out, nil
}

func dump(ctx context.Context, cfg Config, conn Conn, snapshot string) (FileEntry, error) {
	path := filepath.Join(cfg.Dir, DumpFile)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: inside the new backup directory.
	if err != nil {
		return FileEntry{}, fmt.Errorf("backup: create dump file: %w", err)
	}
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(f, h)}
	if err := cfg.Tools.Dump(ctx, conn, snapshot, cw); err != nil {
		_ = f.Close()
		return FileEntry{}, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return FileEntry{}, fmt.Errorf("backup: fsync dump: %w", err)
	}
	if err := f.Close(); err != nil {
		return FileEntry{}, err
	}
	if err := syncDir(cfg.Dir); err != nil {
		return FileEntry{}, err
	}
	if cw.n == 0 {
		return FileEntry{}, errors.New("backup: pg_dump produced no output")
	}
	return FileEntry{Name: DumpFile, SHA256: hex.EncodeToString(h.Sum(nil)), Size: cw.n}, nil
}

// copyBlob copies one blob between stores, re-hashing it on the way. The
// copy lands under its hash only if the bytes match it.
func copyBlob(src, dst blob.Store, sum blob.Sum, size int64) error {
	r, err := src.Open(sum)
	if err != nil {
		return fmt.Errorf("backup: blob %s: %w", sum, err)
	}
	defer func() { _ = r.Close() }()
	key := uuid.Must(uuid.NewV7()).String()
	w, err := dst.CreateStaging(key)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), r)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	var got blob.Sum
	copy(got[:], h.Sum(nil))
	switch {
	case err != nil:
		err = fmt.Errorf("backup: copy blob %s: %w", sum, err)
	case got != sum || n != size:
		err = fmt.Errorf("backup: blob %s does not match its hash or size", sum)
	default:
		err = dst.Promote(key, sum)
	}
	if err != nil {
		_ = dst.RemoveStaging(key)
	}
	return err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func copyN(dst io.Writer, src io.Reader) (int64, error) { return io.Copy(dst, src) }

// prepareEmptyDir creates dir, or accepts an existing empty one.
func prepareEmptyDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("backup: the directory must be an absolute path")
	}
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("backup: create directory: %w", err)
		}
		return syncDir(filepath.Dir(dir))
	case err != nil:
		return fmt.Errorf("backup: read directory: %w", err)
	case len(entries) > 0:
		return fmt.Errorf("backup: %s is not empty", dir)
	}
	return nil
}
