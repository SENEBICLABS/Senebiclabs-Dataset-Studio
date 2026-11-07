package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/backup"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/config"
)

// Environment variables of the backup subcommands (plan 6.2: backup runs as
// studio_backup; restore as the migrator, with verification reads as
// studio_backup and its writes as studio_app).
const (
	envBackupURL   = "STUDIO_BACKUP_DATABASE_URL"
	envAppURL      = "STUDIO_DATABASE_URL"
	envPgContainer = "STUDIO_PG_CONTAINER" // the PostgreSQL 18 container whose tools are used
	backupLimit    = 2 * time.Hour
)

func backupDir(args []string, stderr io.Writer) (string, bool) {
	if len(args) != 1 || args[0] == "" {
		_, _ = fmt.Fprint(stderr, usage)
		return "", false
	}
	dir, err := filepath.Abs(args[0])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return "", false
	}
	return dir, true
}

func requireValue(lookup config.Lookup, key string) (string, error) {
	v := get(lookup, key)
	if v == "" {
		return "", fmt.Errorf("%s is not set", key)
	}
	return v, nil
}

// runBackup is `studioctl backup <dir>` (plan 13).
func runBackup(ctx context.Context, args []string, lookup config.Lookup, stdout, stderr io.Writer) int {
	dir, ok := backupDir(args, stderr)
	if !ok {
		return 2
	}
	url, err := requireURL(lookup, envBackupURL)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	root, err1 := requireValue(lookup, config.EnvBlobRoot)
	container, err2 := requireValue(lookup, envPgContainer)
	if err1 != nil || err2 != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", errorsJoin(err1, err2))
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, backupLimit)
	defer cancel()
	m, err := backup.Backup(ctx, backup.Config{
		BackupURL: url, BlobRoot: root, Dir: dir, Tools: backup.DockerTools{Container: container},
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backup failed (no COMPLETE marker was written): %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "backup complete: %s (schema %d, %d blobs, dump %d bytes)\n", dir, m.GooseVersion, len(m.Blobs), m.Dump.Size)
	return 0
}

// runVerify is `studioctl verify <dir>`: it checks a backup's integrity
// without touching any database.
func runVerify(args []string, stdout, stderr io.Writer) int {
	dir, ok := backupDir(args, stderr)
	if !ok {
		return 2
	}
	m, err := backup.Verify(dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verify failed: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "backup verified: %s (schema %d, %d blobs, created %s)\n", dir, m.GooseVersion, len(m.Blobs), m.CreatedAt.Format(time.RFC3339))
	return 0
}

// runRestore is `studioctl restore <dir>` (plan 13).
func runRestore(ctx context.Context, args []string, lookup config.Lookup, stdout, stderr io.Writer) int {
	dir, ok := backupDir(args, stderr)
	if !ok {
		return 2
	}
	var urls [4]string
	var errs []error
	for i, key := range []string{envBootstrapURL, envMigratorURL, envBackupURL, envAppURL} {
		u, err := requireURL(lookup, key)
		urls[i] = u
		if err != nil {
			errs = append(errs, err)
		}
	}
	root, err := requireValue(lookup, config.EnvBlobRoot)
	if err != nil {
		errs = append(errs, err)
	}
	container, err := requireValue(lookup, envPgContainer)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		_, _ = fmt.Fprintln(stderr, errorsJoin(errs...))
		return 1
	}
	osUser := ""
	if u, err := user.Current(); err == nil {
		osUser = u.Username
	}
	host, _ := os.Hostname()
	ctx, cancel := context.WithTimeout(ctx, backupLimit)
	defer cancel()
	res, err := backup.Restore(ctx, backup.RestoreConfig{
		Dir: dir, BootstrapURL: urls[0], MigratorURL: urls[1], BackupURL: urls[2], AppURL: urls[3],
		BlobRoot: root, Tools: backup.DockerTools{Container: container}, OSUser: osUser, Hostname: host,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "restore failed: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "restore complete and verified: installation %s, schema %d, %d blobs; system.restored event %s\n",
		res.Manifest.InstallationID, res.Manifest.GooseVersion, len(res.Manifest.Blobs), res.EventID)
	return 0
}
